package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/BigRedS/mso-planner/internal/db"
	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
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

var osrmRoadRegexp = regexp.MustCompile(`\b([MA]\d+(?:\([^)]*\))?)\b`)

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
		Roads:       extractRoadNames(r.StepNames),
		Distance:    r.Distance,
		Duration:    r.Duration,
		DistanceKM:  r.Distance / 1000,
		DurationHrs: r.Duration / 3600,
	}, nil
}

func extractRoadNames(stepNames []string) []string {
	seen := make(map[string]bool)
	var roads []string

	for _, name := range stepNames {
		road := normalizeRoadName(name)
		if road == "" {
			continue
		}
		if !seen[road] {
			seen[road] = true
			roads = append(roads, road)
		}
	}

	return roads
}

func normalizeRoadName(name string) string {
	match := osrmRoadRegexp.FindStringSubmatch(name)
	if len(match) < 2 {
		return ""
	}
	return strings.ToUpper(match[1])
}

const routeMatchDistanceMeters = 5000
const earthRadiusMeters = 6371000.0

func filterServicesByRoute(ctx context.Context, pool *db.Pool, gc *geo.Geocoder, services []models.ServiceArea, route routeData) ([]models.ServiceArea, error) {
	var out []models.ServiceArea
	for i := range services {
		sa := &services[i]
		if sa.Latitude == 0 && sa.Longitude == 0 && sa.Postcode != "" {
			if err := maybeGeocodeServiceCoordinates(ctx, pool, gc, sa); err != nil {
				log.Printf("warning: could not geocode %s: %v", sa.Slug, err)
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
		log.Printf("template error: %v", err)
	}
}
