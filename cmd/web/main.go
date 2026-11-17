package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/BigRedS/mso-planner/internal/db"
	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
	"github.com/BigRedS/mso-planner/internal/planner"
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

// main configures the application dependencies and starts the HTTP server.
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	dsn := os.Getenv("MSO_DB_DSN")
	if dsn == "" {
		log.Fatal("MSO_DB_DSN environment variable is required")
	}

	geoCfg, err := geo.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("Database connection failed: %v", err)
	}
	defer pool.Close()

	log.Println("Running migrations...")
	if err := pool.Migrate(ctx); err != nil {
		log.Fatalf("Migration failed: %v", err)
	}

	http.HandleFunc("/", makeHandler(pool, geo.NewGeocoder(geoCfg, pool), geo.NewRouter(geoCfg)))
	http.HandleFunc("/favicon.ico", http.NotFound)

	addr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	log.Printf("Dashboard listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
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

		listed, err := pool.ListServices(ctx, db.ListServicesOptions{
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

		services := wrapServices(listed)
		if len(route.Geometry) > 0 {
			// With a route planned, only the honest answer will do: if nothing
			// matches, the table is empty rather than silently reverting to
			// the unfiltered road list. Services are matched on coordinates
			// captured at scrape time — none are geocoded here, while a user
			// waits; services without coordinates simply don't appear.
			services = planner.ServicesNearRoute(listed, route.Geometry)
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
		log.Printf("template error: %v", err)
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
