package main

import (
	"bytes"
	"io"
	"net/url"
	"strings"
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

func TestParsePreferences(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  planner.Preferences
	}{
		{"nothing set", "", planner.Preferences{}},
		{"form checkboxes", "fuel=on&ev=on", planner.Preferences{RequireFuel: true, RequireEV: true}},
		{"hand-written URL", "fuel=1", planner.Preferences{RequireFuel: true}},
		{"other values are off", "fuel=yes&ev=0", planner.Preferences{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := parsePreferences(q); got != tt.want {
				t.Errorf("parsePreferences(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

// TestTemplateKeepsPreferences checks the form re-renders with the
// preferences that produced the results, so a second search keeps them.
func TestTemplateKeepsPreferences(t *testing.T) {
	var buf bytes.Buffer
	data := dashboardData{Prefs: planner.Preferences{RequireEV: true}}
	if err := templates.ExecuteTemplate(&buf, "index.html", data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if !strings.Contains(html, `name="ev" checked`) {
		t.Error("EV checkbox not checked")
	}
	if strings.Contains(html, `name="fuel" checked`) {
		t.Error("fuel checkbox checked, want unchecked")
	}
}
