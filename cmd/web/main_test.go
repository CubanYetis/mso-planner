package main

import (
	"bytes"
	"io"
	"net/url"
	"regexp"
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
		{"form selects", "fuel=require&ev=prefer&dog=any", planner.Preferences{Fuel: planner.Require, EV: planner.Prefer}},
		{"dog walk", "dog=prefer", planner.Preferences{DogWalk: planner.Prefer}},
		{"old checkbox links mean require", "fuel=on&ev=1", planner.Preferences{Fuel: planner.Require, EV: planner.Require}},
		{"other values mean don't care", "fuel=yes&ev=0&dog=REQUIRE", planner.Preferences{}},
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
	prefs := planner.Preferences{EV: planner.Require, DogWalk: planner.Prefer}
	if err := templates.ExecuteTemplate(&buf, "index.html", dashboardData{PrefControls: prefControls(prefs)}); err != nil {
		t.Fatal(err)
	}

	// Round trip: read back which option each select has selected, and
	// parse that as if the form were submitted.
	selected := regexp.MustCompile(`(?s)<select id="pref-\w+" name="(\w+)">.*?(?:<option value="(\w+)" selected>|</select>)`)
	q := url.Values{}
	for _, m := range selected.FindAllStringSubmatch(buf.String(), -1) {
		if m[2] != "" {
			q.Set(m[1], m[2])
		}
	}
	if got := parsePreferences(q); got != prefs {
		t.Errorf("form re-submits as %+v, want %+v (selected: %v)", got, prefs, q)
	}
}

// TestTemplateShowsRankAndReasons checks the route table shows each
// stop's rank and its badges.
func TestTemplateShowsRankAndReasons(t *testing.T) {
	var buf bytes.Buffer
	data := dashboardData{
		Services: []planner.RouteService{{
			ServiceArea: models.ServiceArea{Name: "Test Services"},
			Rank:        3,
			Reasons:     []string{"EV charging", "Rated 4.0"},
		}},
		Route: routeData{Geometry: []geo.Point{{Lat: 52, Lon: -1}, {Lat: 53, Lon: -1}}},
	}
	if err := templates.ExecuteTemplate(&buf, "index.html", data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"<td>#3</td>", `<span class="badge">EV charging</span>`, `<span class="badge">Rated 4.0</span>`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
}
