package main

import (
	"math"
	"testing"
)

func TestNormalizeRoadName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"motorway", "M1", "M1"},
		{"a road", "A14", "A14"},
		{"road in a longer name", "M6 Toll", "M6"},
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
