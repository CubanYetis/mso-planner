package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
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

// roadRefRegexp matches a whole road reference, not a substring: the road
// number, an optional "(M)" suffix and an optional "Toll". It must stay
// anchored so that "A1" and "A1(M)", which are different roads, never collapse.
var roadRefRegexp = regexp.MustCompile(`^([AMN]\d+)\s?(\(M\))?(\s?TOLL)?$`)

type dashboardFilters struct {
	Road     string
	Operator string
	WithFuel bool
	WithEV   bool
	Start    string
	End      string
	Limit    int
}

type routePoint = geo.Point

type routeData struct {
	Start       string
	End         string
	StartLat    float64
	StartLon    float64
	EndLat      float64
	EndLon      float64
	Geometry    []routePoint
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
	Services        []models.ServiceArea
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

		services, err := pool.ListServices(ctx, db.ListServicesOptions{
			Road:     filters.Road,
			Roads:    route.Roads,
			Operator: filters.Operator,
			WithFuel: filters.WithFuel,
			WithEV:   filters.WithEV,
			Limit:    filters.Limit,
		})
		if err != nil {
			renderTemplate(w, dashboardData{Error: fmt.Sprintf("could not load services: %v", err)})
			return
		}

		if len(route.Geometry) > 0 {
			filtered, err := filterServicesByRoute(ctx, pool, gc, services, route)
			if err != nil {
				renderTemplate(w, dashboardData{Error: fmt.Sprintf("could not filter services by route: %v", err)})
				return
			}
			if len(filtered) > 0 {
				services = filtered
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
		Roads:       extractRoadNames(r.Steps),
		Distance:    r.Distance,
		Duration:    r.Duration,
		DistanceKM:  r.Distance / 1000,
		DurationHrs: r.Duration / 3600,
	}, nil
}

// extractRoadNames returns the unique motorway and A-road names in route order.
// Road numbers live in a step's Ref (possibly several, joined by ";"); Name is
// only consulted as a fallback because it is usually a street name.
func extractRoadNames(steps []geo.Step) []string {
	seen := make(map[string]bool)
	var roads []string

	for _, step := range steps {
		candidates := strings.Split(step.Ref, ";")
		candidates = append(candidates, step.Name)
		for _, c := range candidates {
			road := normalizeRoadName(c)
			if road == "" || seen[road] {
				continue
			}
			seen[road] = true
			roads = append(roads, road)
		}
	}

	return roads
}

// normalizeRoadName returns the canonical form of a road reference as stored in
// service_roads.road ("M1", "A1(M)", "M6 Toll"), or "" if name is not exactly
// a road reference.
func normalizeRoadName(name string) string {
	m := roadRefRegexp.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(name)))
	if m == nil {
		return ""
	}
	road := m[1]
	if m[2] != "" {
		road += "(M)"
	}
	if m[3] != "" {
		road += " Toll"
	}
	return road
}

const routeMatchDistanceMeters = 5000
const earthRadiusMeters = 6371000.0

// filterServicesByRoute returns services within the configured distance of a route.
func filterServicesByRoute(ctx context.Context, pool *db.Pool, gc *geo.Geocoder, services []models.ServiceArea, route routeData) ([]models.ServiceArea, error) {
	var out []models.ServiceArea
	for i := range services {
		sa := &services[i]
		if sa.Latitude == 0 && sa.Longitude == 0 && sa.Postcode != "" {
			if err := maybeGeocodeServiceCoordinates(ctx, pool, gc, sa); err != nil {
				slog.Warn("could not geocode service", "slug", sa.Slug, "error", err)
			}
		}

		if sa.Latitude == 0 && sa.Longitude == 0 {
			continue
		}

		if pointToRouteDistance(sa.Latitude, sa.Longitude, route.Geometry) <= routeMatchDistanceMeters {
			out = append(out, *sa)
		}
	}
	return out, nil
}

// maybeGeocodeServiceCoordinates fills and persists coordinates for a service.
func maybeGeocodeServiceCoordinates(ctx context.Context, pool *db.Pool, gc *geo.Geocoder, sa *models.ServiceArea) error {
	lat, lon, err := gc.Geocode(ctx, sa.Postcode)
	if err != nil {
		return err
	}
	sa.Latitude = lat
	sa.Longitude = lon
	return pool.UpdateServiceCoordinates(ctx, sa.Slug, lat, lon)
}

func pointToRouteDistance(lat, lon float64, geometry []routePoint) float64 {
	if len(geometry) == 0 {
		return math.MaxFloat64
	}
	minDist := math.MaxFloat64
	for i := 0; i < len(geometry)-1; i++ {
		d := distancePointToSegmentMeters(lat, lon, geometry[i], geometry[i+1])
		if d < minDist {
			minDist = d
		}
	}
	return minDist
}

func distancePointToSegmentMeters(lat, lon float64, a, b routePoint) float64 {
	if a.Lat == b.Lat && a.Lon == b.Lon {
		return haversineMeters(lat, lon, a.Lat, a.Lon)
	}

	latR := degreesToRadians(lat)
	lonR := degreesToRadians(lon)
	aLatR := degreesToRadians(a.Lat)
	aLonR := degreesToRadians(a.Lon)
	bLatR := degreesToRadians(b.Lat)
	bLonR := degreesToRadians(b.Lon)
	avgLat := (aLatR + bLatR) / 2

	x1 := (aLonR - lonR) * math.Cos(avgLat)
	y1 := aLatR - latR
	x2 := (bLonR - lonR) * math.Cos(avgLat)
	y2 := bLatR - latR

	dx := x2 - x1
	dy := y2 - y1
	t := -(x1*dx + y1*dy) / (dx*dx + dy*dy)
	if t < 0 {
		return haversineMeters(lat, lon, a.Lat, a.Lon)
	}
	if t > 1 {
		return haversineMeters(lat, lon, b.Lat, b.Lon)
	}

	x := x1 + t*dx
	y := y1 + t*dy
	return math.Sqrt(x*x+y*y) * earthRadiusMeters
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	phi1 := degreesToRadians(lat1)
	phi2 := degreesToRadians(lat2)
	deltaPhi := degreesToRadians(lat2 - lat1)
	deltaLambda := degreesToRadians(lon2 - lon1)

	a := math.Sin(deltaPhi/2)*math.Sin(deltaPhi/2) +
		math.Cos(phi1)*math.Cos(phi2)*math.Sin(deltaLambda/2)*math.Sin(deltaLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMeters * c
}

func degreesToRadians(deg float64) float64 {
	return deg * math.Pi / 180
}

func renderTemplate(w http.ResponseWriter, data dashboardData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, "template rendering failed", http.StatusInternalServerError)
		slog.Error("template rendering failed", "error", err)
	}
}
