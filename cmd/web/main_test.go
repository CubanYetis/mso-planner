package main

import (
	"math"
	"reflect"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
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
		want     float64
		tol      float64
	}{
		{"on the segment", 52, -0.95, 0, 1},
		{"beside the middle", 52.01, -0.95, 1112, 15},
		{"beyond end a", 52, -1.01, 685, 15},
		{"beyond end b", 52, -0.89, 685, 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := distancePointToSegmentMeters(tt.lat, tt.lon, a, b)
			if math.Abs(got-tt.want) > tt.tol {
				t.Errorf("got %.0f m, want %.0f m (+/- %.0f)", got, tt.want, tt.tol)
			}
		})
	}

	t.Run("degenerate segment is a point", func(t *testing.T) {
		got := distancePointToSegmentMeters(52.01, -1, a, a)
		if math.Abs(got-1112) > 15 {
			t.Errorf("got %.0f m, want ~1112", got)
		}
	})
}

func TestPointToRouteDistance(t *testing.T) {
	if got := pointToRouteDistance(52, -1, nil); got != math.MaxFloat64 {
		t.Errorf("empty route = %v, want MaxFloat64", got)
	}

	route := []routePoint{{Lat: 52, Lon: -1}, {Lat: 52, Lon: -0.9}, {Lat: 52.1, Lon: -0.9}}
	// Nearest to the second (north-south) segment, not the first.
	got := pointToRouteDistance(52.05, -0.89, route)
	if got > 800 {
		t.Errorf("got %.0f m, want < 800 (~685)", got)
	}
}
