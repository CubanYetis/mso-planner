package planner

import (
	"math"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
)

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
	a := geo.Point{Lat: 52, Lon: -1}
	b := geo.Point{Lat: 52, Lon: -0.9}

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
	if _, ok := nearestOnRoute(52, -1, "both", nil); ok {
		t.Error("empty route: ok = true, want false")
	}
	if _, ok := nearestOnRoute(52, -1, "both", []geo.Point{{Lat: 52, Lon: -1}}); ok {
		t.Error("single point: ok = true, want false")
	}

	route := []geo.Point{{Lat: 52, Lon: -1}, {Lat: 52, Lon: -0.9}, {Lat: 52.1, Lon: -0.9}}

	// Nearest to the second (north-south) segment, not the first.
	m, ok := nearestOnRoute(52.05, -0.89, "both", route)
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

func TestBearingDegrees(t *testing.T) {
	origin := geo.Point{Lat: 52, Lon: -1}
	tests := []struct {
		name string
		to   geo.Point
		want float64
	}{
		{"due north", geo.Point{Lat: 53, Lon: -1}, 0},
		{"due east", geo.Point{Lat: 52, Lon: 0}, 90},
		{"due south", geo.Point{Lat: 51, Lon: -1}, 180},
		{"due west", geo.Point{Lat: 52, Lon: -2}, 270},
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

// TestNearestOnRouteSkipsWrongHeadingSegment is a regression test: a route
// that doubles back past a service must match the segment heading the right
// way, even though the wrong-way segment is closer.
func TestNearestOnRouteSkipsWrongHeadingSegment(t *testing.T) {
	// Goes north along lon -1, then returns south along lon -0.99.
	route := []geo.Point{{Lat: 52, Lon: -1}, {Lat: 52.1, Lon: -1}, {Lat: 52.1, Lon: -0.99}, {Lat: 52, Lon: -0.99}}
	// Nearer the southbound leg, but signed northbound.
	m, ok := nearestOnRoute(52.05, -0.9905, "northbound", route)
	if !ok {
		t.Fatal("ok = false, want a match on the northbound leg")
	}
	if math.Abs(m.bearing-0) > 1 && math.Abs(m.bearing-360) > 1 {
		t.Errorf("bearing = %.1f, want ~0 (northbound leg)", m.bearing)
	}
	wantAlong := haversineMeters(52, -1, 52.05, -1)
	if math.Abs(m.alongMeters-wantAlong) > 15 {
		t.Errorf("along = %.0f m, want %.0f m", m.alongMeters, wantAlong)
	}

	if _, ok := nearestOnRoute(52.05, -1, "eastbound", []geo.Point{{Lat: 52, Lon: -1}, {Lat: 53, Lon: -1}}); ok {
		t.Error("no eastbound segment: ok = true, want false")
	}
}
