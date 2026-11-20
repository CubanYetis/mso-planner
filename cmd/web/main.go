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
	WithFuel bool
	WithEV   bool
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

		// With a route, the limit applies to route-ordered matches below, not to
		// the candidates: limiting by name first would drop services that are on
		// the route in favour of alphabetically earlier ones that are not.
		listLimit := filters.Limit
		if len(route.Geometry) > 0 {
			listLimit = maxListLimit
		}
		listed, err := pool.ListServices(ctx, db.ListServicesOptions{
			Road:     filters.Road,
			Roads:    route.Roads,
			Operator: filters.Operator,
			WithFuel: filters.WithFuel,
			WithEV:   filters.WithEV,
			Limit:    listLimit,
		})
		if err != nil {
			renderTemplate(w, dashboardData{Error: fmt.Sprintf("could not load services: %v", err)})
			return
		}

		services := wrapServices(listed)
		if len(route.Geometry) > 0 {
			// With a route planned, only the honest answer will do: if nothing
			// matches, the table is empty rather than silently reverting to
			// the unfiltered road list. Services are matched on coordinates
			// captured at scrape time — none are geocoded here, while a user
			// waits; services without coordinates simply don't appear.
			services = planner.ServicesNearRoute(listed, route.Geometry)
			if len(services) > filters.Limit {
				services = services[:filters.Limit]
			}
		}

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
		WithFuel: query.Get("fuel") == "on" || query.Get("fuel") == "1",
		WithEV:   query.Get("ev") == "on" || query.Get("ev") == "1",
		Start:    strings.TrimSpace(query.Get("start")),
		End:      strings.TrimSpace(query.Get("end")),
		Limit:    limit,
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

// wrapServices lists services without route positions, for when no route
// was planned.
func wrapServices(services []models.ServiceArea) []planner.RouteService {
	out := make([]planner.RouteService, len(services))
	for i, sa := range services {
		out[i] = planner.RouteService{ServiceArea: sa}
	}
	return out
}
