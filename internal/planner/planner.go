// Package planner decides which services are useful on a planned route, and
// in what order.
//
// It is pure: callers fetch the services (from the database) and the route
// (from OSRM) and pass them in. Nothing here does I/O, so everything can be
// tested with plain values. Keep it that way: the web UI is one caller, and
// a CLI or saved profiles would be others.
package planner

import (
	"math"
	"sort"

	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
)

// RouteService is a ServiceArea plus where it falls on the planned route,
// so callers can show stops in journey order, and how it ranks for the
// traveller. AlongMeters is 0 when no route was planned; Rank and Reasons
// are unset until Rank is called.
type RouteService struct {
	models.ServiceArea
	AlongMeters float64
	Rank        int      // 1 is the best stop on the route
	Reasons     []string // why it ranks where it does, for badges

}

// AlongKM is AlongMeters in kilometres, for display.
func (s RouteService) AlongKM() float64 {
	return s.AlongMeters / 1000
}

const routeMatchDistanceMeters = 5000

// ServicesNearRoute returns the services within the matching distance of the
// route that serve the direction of travel, ordered by how far along the
// route their closest point falls. Services without coordinates can't be
// placed and are dropped.
func ServicesNearRoute(services []models.ServiceArea, geometry []geo.Point) []RouteService {
	var out []RouteService
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
		out = append(out, RouteService{ServiceArea: sa, AlongMeters: m.alongMeters})
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
