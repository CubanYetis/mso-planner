package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
	"github.com/BigRedS/mso-planner/internal/planner"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	// Planning a route makes two sequential geocode calls plus one routing
	// call, each with its own ~12s upstream timeout, so this needs headroom.
	writeTimeout    = 40 * time.Second
	idleTimeout     = 2 * time.Minute
	shutdownTimeout = 10 * time.Second
)

//go:embed templates/*.html
var templatesFS embed.FS

var templates = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"join": strings.Join,
	"yesno": func(v bool) string {
		if v {
			return "Yes"
		}
		return "No"
	},
}).ParseFS(templatesFS, "templates/*.html"))

// maxListLimit is the most services ListServices will return; it matches the
// cap inside db.ListServices.
const maxListLimit = 1000

type dashboardFilters struct {
	Road     string
	Operator string
	Start    string
	End      string
	Limit    int
}

type routeData struct {
	Start       string
	End         string
	StartLat    float64
	StartLon    float64
	EndLat      float64
	EndLon      float64
	Geometry    []geo.Point
	Roads       []string
	Distance    float64
	Duration    float64
	DistanceKM  float64
	DurationHrs float64
}

type dashboardData struct {
	Total           int
	WithFuel        int
	WithEV          int
	Filters         dashboardFilters
	PrefControls    []prefControl
	Services        []planner.RouteService
	Error           string
	Route           routeData
	RouteGeometryJS template.JS
	RouteError      string
}

// main configures the application dependencies and starts the HTTP server. It
// shuts down gracefully on SIGINT/SIGTERM, finishing in-flight requests
// before the process exits.
func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dsn := os.Getenv("MSO_DB_DSN")
	if dsn == "" {
		logger.Error("MSO_DB_DSN environment variable is required")
		os.Exit(1)
	}

	geoCfg, err := geo.ConfigFromEnv()
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	logger.Info("running migrations")
	if err := pool.Migrate(ctx); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", makeHandler(pool, geo.NewGeocoder(geoCfg, pool), geo.NewRouter(geoCfg)))
	mux.HandleFunc("/healthz", makeHealthzHandler(pool))
	mux.HandleFunc("/favicon.ico", http.NotFound)

	addr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("dashboard listening", "addr", addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down: signal received")
		stop() // restore default signal behaviour so a second signal force-quits

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
		logger.Info("shut down cleanly")
	}
}

// pinger is satisfied by *db.Pool; declared narrowly here so /healthz is
// testable without a real database.
type pinger interface {
	Ping(ctx context.Context) error
}

// makeHealthzHandler reports whether the database is reachable. It is meant
// for a Kubernetes liveness/readiness probe.
func makeHealthzHandler(p pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := p.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "database unreachable: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	}
}

// makeHandler builds the dashboard handler with its database and geo clients.
func makeHandler(pool *db.Pool, gc *geo.Geocoder, rt *geo.Router) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filters := parseFilters(r)
		prefs := parsePreferences(r.URL.Query())
		ctx := r.Context()

		total, withFuel, withEV, err := pool.Stats(ctx)
		if err != nil {
			renderTemplate(w, dashboardData{Error: fmt.Sprintf("could not load stats: %v", err)})
			return
		}

		var route routeData
		var routeError string
		if filters.Start != "" && filters.End != "" {
			route, err = planRoute(ctx, gc, rt, filters.Start, filters.End)
			if err != nil {
				routeError = fmt.Sprintf("route error: %v", err)
			}
		}

		// The limit applies to the final results (see selectServices), not to the
		// candidates: limiting by name in SQL first would spend the slots on
		// services that the route match or the preferences then drop.
		listed, err := pool.ListServices(ctx, db.ListServicesOptions{
			Road:     filters.Road,
			Roads:    route.Roads,
			Operator: filters.Operator,
			// planner.Filter below is what decides; this only stops the SQL
			// LIMIT being spent on services that would be filtered out anyway.
			WithFuel: prefs.Fuel == planner.Require,
			WithEV:   prefs.EV == planner.Require,
			Limit:    maxListLimit,
		})
		if err != nil {
			renderTemplate(w, dashboardData{Error: fmt.Sprintf("could not load services: %v", err)})
			return
		}

		services := selectServices(listed, route.Geometry, prefs, filters.Limit)

		routeJSON := template.JS("null")
		if len(route.Geometry) > 0 {
			b, err := json.Marshal(route.Geometry)
			if err == nil {
				routeJSON = template.JS(b)
			}
		}

		renderTemplate(w, dashboardData{
			Total:           total,
			WithFuel:        withFuel,
			WithEV:          withEV,
			Filters:         filters,
			PrefControls:    prefControls(prefs),
			Services:        services,
			Route:           route,
			RouteGeometryJS: routeJSON,
			RouteError:      routeError,
		})
	}
}

// parseFilters reads the dashboard filters from the query string.
func parseFilters(r *http.Request) dashboardFilters {
	query := r.URL.Query()
	limit := 200
	if v := query.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	return dashboardFilters{
		Road:     strings.ToUpper(strings.TrimSpace(query.Get("road"))),
		Operator: strings.TrimSpace(query.Get("operator")),
		Start:    strings.TrimSpace(query.Get("start")),
		End:      strings.TrimSpace(query.Get("end")),
		Limit:    limit,
	}
}

// prefControl is one facility's don't care/prefer/require select on the
// search form.
type prefControl struct {
	Name  string // query parameter, as read by parsePreferences
	Label string
	Value planner.Want
}

func prefControls(p planner.Preferences) []prefControl {
	return []prefControl{
		{"fuel", "Fuel", p.Fuel},
		{"ev", "EV charging", p.EV},
		{"dog", "Dog walk", p.DogWalk},
	}
}

// parsePreferences reads the traveller's preferences from the search form:
// "prefer" or "require" per facility, anything else meaning don't care.
// "on" and "1" mean require, so links from when these were checkboxes still
// work.
func parsePreferences(query url.Values) planner.Preferences {
	return planner.Preferences{
		Fuel:    parseWant(query.Get("fuel")),
		EV:      parseWant(query.Get("ev")),
		DogWalk: parseWant(query.Get("dog")),
	}
}

func parseWant(v string) planner.Want {
	switch v {
	case "prefer":
		return planner.Prefer
	case "require", "on", "1":
		return planner.Require
	default:
		return planner.DontCare
	}
}

// planRoute geocodes the endpoints and requests a driving route between them.
func planRoute(ctx context.Context, gc *geo.Geocoder, rt *geo.Router, start, end string) (routeData, error) {
	startLat, startLon, err := gc.Geocode(ctx, start)
	if err != nil {
		return routeData{}, fmt.Errorf("could not geocode start location: %w", err)
	}
	endLat, endLon, err := gc.Geocode(ctx, end)
	if err != nil {
		return routeData{}, fmt.Errorf("could not geocode destination: %w", err)
	}

	r, err := rt.Route(ctx, geo.Point{Lat: startLat, Lon: startLon}, geo.Point{Lat: endLat, Lon: endLon})
	if err != nil {
		return routeData{}, err
	}

	return routeData{
		Start:       start,
		End:         end,
		StartLat:    startLat,
		StartLon:    startLon,
		EndLat:      endLat,
		EndLon:      endLon,
		Geometry:    r.Geometry,
		Roads:       planner.ExtractRoadNames(r.Steps),
		Distance:    r.Distance,
		Duration:    r.Duration,
		DistanceKM:  r.Distance / 1000,
		DurationHrs: r.Duration / 3600,
	}, nil
}

func renderTemplate(w http.ResponseWriter, data dashboardData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, "template rendering failed", http.StatusInternalServerError)
		slog.Error("template rendering failed", "error", err)
	}
}

// selectServices turns the candidate services into the table's rows, applying
// limit last so that it can't cut anything the earlier steps would have kept.
//
// With a route, only the honest answer will do: if nothing matches, the table
// is empty rather than silently reverting to the unfiltered road list.
// Services are matched on coordinates captured at scrape time — none are
// geocoded here, while a user waits; services without coordinates simply don't
// appear. The survivors are ranked and the limit keeps the best-ranked, still
// in journey order. Without a route, the first limit survivors are kept.
func selectServices(listed []models.ServiceArea, geometry []geo.Point, prefs planner.Preferences, limit int) []planner.RouteService {
	if len(geometry) == 0 {
		services := planner.Filter(wrapServices(listed), prefs)
		if len(services) > limit {
			services = services[:limit]
		}
		return services
	}

	services := planner.Rank(planner.Filter(planner.ServicesNearRoute(listed, geometry), prefs), prefs)
	var best []planner.RouteService
	for _, s := range services {
		// Ranks are distinct, 1..len, so this keeps exactly limit of them.
		if s.Rank <= limit {
			best = append(best, s)
		}
	}
	return best
}

// wrapServices lists services without route positions, for when no route
// was planned.
func wrapServices(services []models.ServiceArea) []planner.RouteService {
	out := make([]planner.RouteService, len(services))
	for i, sa := range services {
		out[i] = planner.RouteService{ServiceArea: sa}
	}
	return out
}
