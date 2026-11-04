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
	"sort"
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

// routeService is a ServiceArea plus where it falls on the planned route,
// so the template can show stops in journey order. AlongMeters is 0 when no
// route was planned.
type routeService struct {
	models.ServiceArea
	AlongMeters float64
}

func (s routeService) AlongKM() float64 {
	return s.AlongMeters / 1000
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
	Services        []routeService
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

		var services []routeService
		if len(route.Geometry) > 0 {
			services, err = filterServicesByRoute(ctx, pool, gc, listed, route)
			if err != nil {
				renderTemplate(w, dashboardData{Error: fmt.Sprintf("could not filter services by route: %v", err)})
				return
			}
			if len(services) == 0 {
				services = wrapServices(listed)
			}
		} else {
			services = wrapServices(listed)
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

// filterServicesByRoute fills in missing coordinates, then keeps only the
// services near the route, in journey order.
func filterServicesByRoute(ctx context.Context, pool *db.Pool, gc *geo.Geocoder, services []models.ServiceArea, route routeData) ([]routeService, error) {
	for i := range services {
		sa := &services[i]
		if sa.Latitude == 0 && sa.Longitude == 0 && sa.Postcode != "" {
			if err := maybeGeocodeServiceCoordinates(ctx, pool, gc, sa); err != nil {
				log.Printf("warning: could not geocode %s: %v", sa.Slug, err)
			}
		}
	}
	return servicesNearRoute(services, route.Geometry), nil
}

// servicesNearRoute returns the services within the matching distance of the
// route that serve the direction of travel, ordered by how far along the
// route their closest point falls. Services without coordinates can't be
// placed and are dropped.
func servicesNearRoute(services []models.ServiceArea, geometry []routePoint) []routeService {
	var out []routeService
	for _, sa := range services {
		if sa.Latitude == 0 && sa.Longitude == 0 {
			continue
		}
		m, ok := nearestOnRoute(sa.Latitude, sa.Longitude, geometry)
		if !ok || m.distMeters > routeMatchDistanceMeters {
			continue
		}
		if !directionMatchesBearing(sa.Direction, m.bearing) {
			continue
		}
		out = append(out, routeService{ServiceArea: sa, AlongMeters: m.alongMeters})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AlongMeters < out[j].AlongMeters })
	return out
}

// directionConeHalfAngleDegrees is how far the route's heading may drift from
// a service's signed direction before we decide it serves the other
// carriageway. Motorways bend, so the cones are wide.
const directionConeHalfAngleDegrees = 45.0

// directionMatchesBearing reports whether a service signed for direction is
// useful when travelling on the given bearing. "both" (online and junction
// services) always matches, as do clockwise/anticlockwise: deciding those
// needs the ring road's centre and there are few of them, so they stay
// unfiltered for now.
func directionMatchesBearing(direction string, bearing float64) bool {
	var want float64
	switch direction {
	case "northbound":
		want = 0
	case "eastbound":
		want = 90
	case "southbound":
		want = 180
	case "westbound":
		want = 270
	default:
		return true
	}
	diff := math.Abs(math.Mod(bearing-want+540, 360) - 180)
	return diff <= directionConeHalfAngleDegrees
}

func wrapServices(services []models.ServiceArea) []routeService {
	out := make([]routeService, len(services))
	for i, sa := range services {
		out[i] = routeService{ServiceArea: sa}
	}
	return out
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

// routeMatch describes a point's relationship to a route: how far off the
// route it is, where along the route its closest point sits, and which way
// the route heads there.
type routeMatch struct {
	distMeters  float64
	alongMeters float64 // arc length from the route's start to the closest point
	bearing     float64 // compass heading of the winning segment, degrees
}

// nearestOnRoute finds the closest point on geometry to (lat, lon). ok is
// false when the route has no segments to measure against.
func nearestOnRoute(lat, lon float64, geometry []routePoint) (m routeMatch, ok bool) {
	if len(geometry) < 2 {
		return routeMatch{}, false
	}
	m.distMeters = math.MaxFloat64
	var cum float64
	for i := 0; i < len(geometry)-1; i++ {
		segLen := haversineMeters(geometry[i].Lat, geometry[i].Lon, geometry[i+1].Lat, geometry[i+1].Lon)
		d, t := distancePointToSegmentMeters(lat, lon, geometry[i], geometry[i+1])
		if d < m.distMeters {
			m.distMeters = d
			m.alongMeters = cum + t*segLen
			m.bearing = bearingDegrees(geometry[i], geometry[i+1])
		}
		cum += segLen
	}
	return m, true
}

// distancePointToSegmentMeters returns the distance from (lat, lon) to segment
// a-b and the clamped projection parameter t: 0 means the closest point is a,
// 1 means b, and values in between are that fraction along the segment.
func distancePointToSegmentMeters(lat, lon float64, a, b routePoint) (dist float64, t float64) {
	if a.Lat == b.Lat && a.Lon == b.Lon {
		return haversineMeters(lat, lon, a.Lat, a.Lon), 0
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
	t = -(x1*dx + y1*dy) / (dx*dx + dy*dy)
	if t < 0 {
		return haversineMeters(lat, lon, a.Lat, a.Lon), 0
	}
	if t > 1 {
		return haversineMeters(lat, lon, b.Lat, b.Lon), 1
	}

	x := x1 + t*dx
	y := y1 + t*dy
	return math.Sqrt(x*x+y*y) * earthRadiusMeters, t
}

// bearingDegrees returns the initial compass bearing from a to b, clockwise
// from north.
func bearingDegrees(a, b routePoint) float64 {
	lat1, lat2 := degreesToRadians(a.Lat), degreesToRadians(b.Lat)
	dLon := degreesToRadians(b.Lon - a.Lon)
	y := math.Sin(dLon) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLon)
	return math.Mod(math.Atan2(y, x)*180/math.Pi+360, 360)
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
