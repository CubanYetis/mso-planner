package main

import (
	"math"
	"reflect"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
)

func TestNormalizeRoadName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"motorway", "M1", "M1"},
		{"a road", "A14", "A14"},
		{"a road with a letter-free number", "A414", "A414"},
		{"motorway-grade a road", "A1(M)", "A1(M)"},
		{"a1 and a1(m) stay distinct", "A1", "A1"},
		{"toll road keeps its suffix", "M6 Toll", "M6 Toll"},
		{"lowercase and padding", "  a1(m) ", "A1(M)"},
		{"space before (M)", "A1 (M)", "A1(M)"},
		{"irish motorway is not plain M1", "M1 (Ireland)", ""},
		{"road number inside a name", "North Orbital Road A405", ""},
		{"empty", "", ""},
		{"unnumbered street", "High Street", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeRoadName(tt.in); got != tt.want {
				t.Errorf("normalizeRoadName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestExtractRoadNames uses steps captured from the public OSRM server for
// N3 3DU to AL2 2AB. Road numbers are in ref and name is empty on the M1,
// which is why reading only name found no roads at all.
func TestExtractRoadNames(t *testing.T) {
	steps := []geo.Step{
		{Name: "Lichfield Grove"},
		{Name: "Regent's Park Road", Ref: "A598"},
		{Name: ""},
		{Name: "Hendon Lane", Ref: "A5000"},
		{Name: "Great North Way", Ref: "A1"},
		{Name: "", Ref: "M1"},
		{Name: "", Ref: "M1"},
		{Name: "North Orbital Road", Ref: "A405"},
		{Name: "North Orbital Road", Ref: "A405"},
		{Name: "Tippendell Lane"},
		{Name: "Park Street", Ref: "A5183"},
	}
	got := extractRoadNames(steps)
	want := []string{"A598", "A5000", "A1", "M1", "A405", "A5183"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (unique, in route order)", got, want)
	}
}

func TestExtractRoadNamesMultipleRefs(t *testing.T) {
	steps := []geo.Step{
		{Name: "", Ref: "A1(M);A1"},
		{Name: "", Ref: "A1"},
		{Name: "M6 Toll"},
		{Name: "", Ref: "E15;M1"},
	}
	got := extractRoadNames(steps)
	want := []string{"A1(M)", "A1", "M6 Toll", "M1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (A1(M) and A1 kept separate; non-UK refs dropped)", got, want)
	}
}

func TestHaversineMeters(t *testing.T) {
	// London to Birmingham is roughly 163 km in a straight line.
	got := haversineMeters(51.5074, -0.1278, 52.4862, -1.8904)
	if math.Abs(got-163000) > 3000 {
		t.Errorf("London-Birmingham = %.0f m, want ~163000", got)
	}
	if d := haversineMeters(52, -1, 52, -1); d != 0 {
		t.Errorf("same point = %f, want 0", d)
	}
}

func TestDistancePointToSegmentMeters(t *testing.T) {
	// A west-east segment along latitude 52; 0.01 degrees of latitude is ~1112 m.
	a := routePoint{Lat: 52, Lon: -1}
	b := routePoint{Lat: 52, Lon: -0.9}

	tests := []struct {
		name     string
		lat, lon float64
		wantDist float64
		wantT    float64
		tol      float64
	}{
		{"on the segment", 52, -0.95, 0, 0.5, 1},
		{"beside the middle", 52.01, -0.95, 1112, 0.5, 15},
		{"beyond end a", 52, -1.01, 685, 0, 15},
		{"beyond end b", 52, -0.89, 685, 1, 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dist, proj := distancePointToSegmentMeters(tt.lat, tt.lon, a, b)
			if math.Abs(dist-tt.wantDist) > tt.tol {
				t.Errorf("dist = %.0f m, want %.0f m (+/- %.0f)", dist, tt.wantDist, tt.tol)
			}
			if math.Abs(proj-tt.wantT) > 0.02 {
				t.Errorf("t = %.2f, want %.2f", proj, tt.wantT)
			}
		})
	}

	t.Run("degenerate segment is a point", func(t *testing.T) {
		dist, proj := distancePointToSegmentMeters(52.01, -1, a, a)
		if math.Abs(dist-1112) > 15 {
			t.Errorf("dist = %.0f m, want ~1112", dist)
		}
		if proj != 0 {
			t.Errorf("t = %.2f, want 0", proj)
		}
	})
}

func TestNearestOnRoute(t *testing.T) {
	if _, ok := nearestOnRoute(52, -1, nil); ok {
		t.Error("empty route: ok = true, want false")
	}
	if _, ok := nearestOnRoute(52, -1, []routePoint{{Lat: 52, Lon: -1}}); ok {
		t.Error("single point: ok = true, want false")
	}

	route := []routePoint{{Lat: 52, Lon: -1}, {Lat: 52, Lon: -0.9}, {Lat: 52.1, Lon: -0.9}}

	// Nearest to the second (north-south) segment, not the first.
	m, ok := nearestOnRoute(52.05, -0.89, route)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if m.distMeters > 800 {
		t.Errorf("dist = %.0f m, want < 800 (~685)", m.distMeters)
	}

	// Along is the first segment plus halfway up the second.
	wantAlong := haversineMeters(52, -1, 52, -0.9) + haversineMeters(52, -0.9, 52.05, -0.9)
	if math.Abs(m.alongMeters-wantAlong) > 15 {
		t.Errorf("along = %.0f m, want %.0f m", m.alongMeters, wantAlong)
	}

	// The winning segment runs due north.
	if math.Abs(m.bearing-0) > 1 && math.Abs(m.bearing-360) > 1 {
		t.Errorf("bearing = %.1f, want ~0", m.bearing)
	}
}

// TestServicesNearRoute uses a due-west-to-east route along latitude 52 and
// expects services in that journey order regardless of input order.
func TestServicesNearRoute(t *testing.T) {
	route := []routePoint{{Lat: 52, Lon: -1}, {Lat: 52, Lon: 0}}
	services := []models.ServiceArea{
		{Slug: "last", Latitude: 52.01, Longitude: -0.2},
		{Slug: "too-far", Latitude: 53, Longitude: -0.5},
		{Slug: "no-coords", Longitude: -0.4},
		{Slug: "first", Latitude: 52.01, Longitude: -0.8},
		{Slug: "middle", Latitude: 52.01, Longitude: -0.5},
	}
	got := servicesNearRoute(services, route)
	var order []string
	for _, s := range got {
		order = append(order, s.Slug)
	}
	want := []string{"first", "middle", "last"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestDirectionMatchesBearing(t *testing.T) {
	tests := []struct {
		name      string
		direction string
		bearing   float64
		want      bool
	}{
		{"northbound heading north", "northbound", 0, true},
		{"northbound on a bend", "northbound", 44, true},
		{"northbound heading east", "northbound", 90, false},
		{"northbound heading south", "northbound", 180, false},
		{"northbound just past midnight", "northbound", 316, true},
		{"southbound heading south", "southbound", 180, true},
		{"southbound heading north", "southbound", 0, false},
		{"eastbound heading east", "eastbound", 95, true},
		{"westbound heading east", "westbound", 90, false},
		{"both always matches", "both", 123, true},
		{"clockwise unfiltered for now", "clockwise", 123, true},
		{"anticlockwise unfiltered for now", "anticlockwise", 123, true},
		{"unrecognised direction is kept", "", 123, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := directionMatchesBearing(tt.direction, tt.bearing); got != tt.want {
				t.Errorf("directionMatchesBearing(%q, %.0f) = %v, want %v", tt.direction, tt.bearing, got, tt.want)
			}
		})
	}
}

// TestServicesNearRouteFiltersDirection drives a due-north route past a
// pair of services and expects only the northbound-facing one to survive.
func TestServicesNearRouteFiltersDirection(t *testing.T) {
	route := []routePoint{{Lat: 52, Lon: -1}, {Lat: 53, Lon: -1}}
	services := []models.ServiceArea{
		{Slug: "northbound-side", Direction: "northbound", Latitude: 52.5, Longitude: -1.001},
		{Slug: "southbound-side", Direction: "southbound", Latitude: 52.5, Longitude: -1.002},
		{Slug: "online-both", Direction: "both", Latitude: 52.6, Longitude: -1.001},
	}
	got := servicesNearRoute(services, route)
	var order []string
	for _, s := range got {
		order = append(order, s.Slug)
	}
	want := []string{"northbound-side", "online-both"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestBearingDegrees(t *testing.T) {
	origin := routePoint{Lat: 52, Lon: -1}
	tests := []struct {
		name string
		to   routePoint
		want float64
	}{
		{"due north", routePoint{Lat: 53, Lon: -1}, 0},
		{"due east", routePoint{Lat: 52, Lon: 0}, 90},
		{"due south", routePoint{Lat: 51, Lon: -1}, 180},
		{"due west", routePoint{Lat: 52, Lon: -2}, 270},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bearingDegrees(origin, tt.to)
			if math.Abs(got-tt.want) > 1 {
				t.Errorf("got %.1f, want %.1f", got, tt.want)
			}
		})
	}
}
