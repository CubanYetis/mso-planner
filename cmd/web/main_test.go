package main

import (
	"io"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
	"github.com/BigRedS/mso-planner/internal/planner"
)

// TestTemplateRenders guards the field references in index.html — a typo in
// the template fails at runtime, not at build time, without this.
func TestTemplateRenders(t *testing.T) {
	data := dashboardData{
		Services: []planner.RouteService{
			{ServiceArea: models.ServiceArea{Name: "Test Services"}, AlongMeters: 42000},
		},
		Route: routeData{Geometry: []geo.Point{{Lat: 52, Lon: -1}, {Lat: 53, Lon: -1}}},
	}
	if err := templates.ExecuteTemplate(io.Discard, "index.html", data); err != nil {
		t.Fatalf("render with route: %v", err)
	}
	if err := templates.ExecuteTemplate(io.Discard, "index.html", dashboardData{}); err != nil {
		t.Fatalf("render without route: %v", err)
	}
}
