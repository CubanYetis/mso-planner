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
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
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

var httpClient = &http.Client{
	Timeout: 12 * time.Second,
}

type dashboardFilters struct {
	Road     string
	Operator string
	WithFuel bool
	WithEV   bool
	Start    string
	End      string
	Limit    int
}

type routePoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

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

	http.HandleFunc("/", makeHandler(pool))
	http.HandleFunc("/favicon.ico", http.NotFound)

	addr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	log.Printf("Dashboard listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func makeHandler(pool *db.Pool) http.HandlerFunc {
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
			route, err = planRoute(ctx, filters.Start, filters.End)
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
			filtered, err := filterServicesByRoute(ctx, pool, services, route)
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

type nominatimResult struct {
	Lat string `json:"lat"`
	Lon string `json:"lon"`
}

type osrmResponse struct {
	Code   string `json:"code"`
	Routes []struct {
		Distance float64 `json:"distance"`
		Duration float64 `json:"duration"`
		Geometry struct {
			Coordinates [][]float64 `json:"coordinates"`
		} `json:"geometry"`
		Legs []struct {
			Steps []struct {
				Name string `json:"name"`
			} `json:"steps"`
		} `json:"legs"`
	} `json:"routes"`
}

func planRoute(ctx context.Context, start, end string) (routeData, error) {
	startLat, startLon, err := geocodeAddress(ctx, start)
	if err != nil {
		return routeData{}, fmt.Errorf("could not geocode start location: %w", err)
	}
	endLat, endLon, err := geocodeAddress(ctx, end)
	if err != nil {
		return routeData{}, fmt.Errorf("could not geocode destination: %w", err)
	}

	route, err := fetchOSRMRoute(ctx, startLat, startLon, endLat, endLon)
	if err != nil {
		return routeData{}, err
	}

	route.Start = start
	route.End = end
	route.StartLat = startLat
	route.StartLon = startLon
	route.EndLat = endLat
	route.EndLon = endLon
	return route, nil
}

func geocodeAddress(ctx context.Context, query string) (float64, float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://nominatim.openstreetmap.org/search", nil)
	if err != nil {
		return 0, 0, err
	}

	q := req.URL.Query()
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("limit", "1")
	q.Set("countrycodes", "gb")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", "MSO-Planner/1.0 (route planner; contact: ialoneambest@gmail.com)")

	res, err := httpClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("geocoding failed: %s", res.Status)
	}

	var results []nominatimResult
	if err := json.NewDecoder(res.Body).Decode(&results); err != nil {
		return 0, 0, err
	}
	if len(results) == 0 {
		return 0, 0, fmt.Errorf("no geocoding results")
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return 0, 0, err
	}
	lon, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return 0, 0, err
	}
	return lat, lon, nil
}

func fetchOSRMRoute(ctx context.Context, startLat, startLon, endLat, endLon float64) (routeData, error) {
	url := fmt.Sprintf("https://router.project-osrm.org/route/v1/driving/%.6f,%.6f;%.6f,%.6f?overview=full&geometries=geojson&steps=true", startLon, startLat, endLon, endLat)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return routeData{}, err
	}
	req.Header.Set("User-Agent", "MSO-Planner/1.0 (route planner; contact: BigRedS)")

	res, err := httpClient.Do(req)
	if err != nil {
		return routeData{}, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return routeData{}, fmt.Errorf("routing failed: %s", res.Status)
	}

	var osrm osrmResponse
	if err := json.NewDecoder(res.Body).Decode(&osrm); err != nil {
		return routeData{}, err
	}
	if osrm.Code != "Ok" || len(osrm.Routes) == 0 {
		return routeData{}, fmt.Errorf("routing response invalid: code=%s", osrm.Code)
	}

	route := routeData{
		Geometry:    make([]routePoint, 0, len(osrm.Routes[0].Geometry.Coordinates)),
		Distance:    osrm.Routes[0].Distance,
		Duration:    osrm.Routes[0].Duration,
		DistanceKM:  osrm.Routes[0].Distance / 1000,
		DurationHrs: osrm.Routes[0].Duration / 3600,
		Roads:       extractRoadNames(&osrm),
	}

	for _, coord := range osrm.Routes[0].Geometry.Coordinates {
		if len(coord) == 2 {
			route.Geometry = append(route.Geometry, routePoint{Lat: coord[1], Lon: coord[0]})
		}
	}

	return route, nil
}

func extractRoadNames(osrm *osrmResponse) []string {
	seen := make(map[string]bool)
	var roads []string

	for _, route := range osrm.Routes {
		for _, leg := range route.Legs {
			for _, step := range leg.Steps {
				road := normalizeRoadName(step.Name)
				if road == "" {
					continue
				}
				if !seen[road] {
					seen[road] = true
					roads = append(roads, road)
				}
			}
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

func filterServicesByRoute(ctx context.Context, pool *db.Pool, services []models.ServiceArea, route routeData) ([]models.ServiceArea, error) {
	var out []models.ServiceArea
	for i := range services {
		sa := &services[i]
		if sa.Latitude == 0 && sa.Longitude == 0 && sa.Postcode != "" {
			if err := maybeGeocodeServiceCoordinates(ctx, pool, sa); err != nil {
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

func maybeGeocodeServiceCoordinates(ctx context.Context, pool *db.Pool, sa *models.ServiceArea) error {
	lat, lon, err := geocodeAddress(ctx, sa.Postcode)
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
