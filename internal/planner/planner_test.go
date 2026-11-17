package planner

import (
	"reflect"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
)

// TestServicesNearRoute uses a due-west-to-east route along latitude 52 and
// expects services in that journey order regardless of input order.
func TestServicesNearRoute(t *testing.T) {
	route := []geo.Point{{Lat: 52, Lon: -1}, {Lat: 52, Lon: 0}}
	services := []models.ServiceArea{
		{Slug: "last", Latitude: 52.01, Longitude: -0.2},
		{Slug: "too-far", Latitude: 53, Longitude: -0.5},
		{Slug: "no-coords", Longitude: -0.4},
		{Slug: "first", Latitude: 52.01, Longitude: -0.8},
		{Slug: "middle", Latitude: 52.01, Longitude: -0.5},
	}
	got := ServicesNearRoute(services, route)
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
	route := []geo.Point{{Lat: 52, Lon: -1}, {Lat: 53, Lon: -1}}
	services := []models.ServiceArea{
		{Slug: "northbound-side", Direction: "northbound", Latitude: 52.5, Longitude: -1.001},
		{Slug: "southbound-side", Direction: "southbound", Latitude: 52.5, Longitude: -1.002},
		{Slug: "online-both", Direction: "both", Latitude: 52.6, Longitude: -1.001},
	}
	got := ServicesNearRoute(services, route)
	var order []string
	for _, s := range got {
		order = append(order, s.Slug)
	}
	want := []string{"northbound-side", "online-both"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}
