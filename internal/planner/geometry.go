package planner

import (
	"math"

	"github.com/BigRedS/mso-planner/internal/geo"
)

const earthRadiusMeters = 6371000.0

// routeMatch describes a point's relationship to a route: how far off the
// route it is, where along the route its closest point sits, and which way
// the route heads there.
type routeMatch struct {
	distMeters  float64
	alongMeters float64 // arc length from the route's start to the closest point
	bearing     float64 // compass heading of the winning segment, degrees
}

// nearestOnRoute finds the closest point on geometry to (lat, lon) among the
// segments whose bearing suits direction, so a nearby segment heading the wrong
// way (a loop, or the other carriageway) can't shadow a matching one. ok is
// false when no segment qualifies.
func nearestOnRoute(lat, lon float64, direction string, geometry []geo.Point) (m routeMatch, ok bool) {
	if len(geometry) < 2 {
		return routeMatch{}, false
	}
	m.distMeters = math.MaxFloat64
	var cum float64
	for i := 0; i < len(geometry)-1; i++ {
		segLen := haversineMeters(geometry[i].Lat, geometry[i].Lon, geometry[i+1].Lat, geometry[i+1].Lon)
		// A zero-length segment has no heading (its bearing comes out as 0,
		// which would pass for northbound), so it can't match any direction.
		bearing := bearingDegrees(geometry[i], geometry[i+1])
		if segLen > 0 && directionMatchesBearing(direction, bearing) {
			d, t := distancePointToSegmentMeters(lat, lon, geometry[i], geometry[i+1])
			if d < m.distMeters {
				m.distMeters = d
				m.alongMeters = cum + t*segLen
				m.bearing = bearing
				ok = true
			}
		}
		cum += segLen
	}
	return m, ok
}

// distancePointToSegmentMeters returns the distance from (lat, lon) to segment
// a-b and the clamped projection parameter t: 0 means the closest point is a,
// 1 means b, and values in between are that fraction along the segment.
func distancePointToSegmentMeters(lat, lon float64, a, b geo.Point) (dist float64, t float64) {
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
func bearingDegrees(a, b geo.Point) float64 {
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
