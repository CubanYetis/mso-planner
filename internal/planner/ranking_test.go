package planner

import (
	"reflect"
	"testing"

	"github.com/BigRedS/mso-planner/internal/models"
)

// svc builds a RouteService for tests from just the fields ranking looks at.
func svc(slug string, rating float32, f models.Facilities) RouteService {
	return RouteService{ServiceArea: models.ServiceArea{Slug: slug, MSOrating: rating, Facilities: f}}
}

func slugs(services []RouteService) []string {
	var out []string
	for _, s := range services {
		out = append(out, s.Slug)
	}
	return out
}

// ranks returns slug -> rank, so tests can state expectations independent
// of the (journey) order Rank returns services in.
func ranks(services []RouteService) map[string]int {
	out := map[string]int{}
	for _, s := range services {
		out[s.Slug] = s.Rank
	}
	return out
}

var (
	fuel    = models.Facilities{FuelBrands: []string{"Shell"}}
	ev      = models.Facilities{EVCharging: true}
	dog     = models.Facilities{DogFriendly: true}
	all     = models.Facilities{FuelBrands: []string{"Shell"}, EVCharging: true, DogFriendly: true}
	nothing = models.Facilities{}
)

func TestFilter(t *testing.T) {
	services := []RouteService{
		svc("fuel", 0, fuel),
		svc("ev", 0, ev),
		svc("dog", 0, dog),
		svc("all", 0, all),
		svc("none", 0, models.Facilities{}),
	}
	tests := []struct {
		name  string
		prefs Preferences
		want  []string
	}{
		{"no preferences keeps everything", Preferences{}, []string{"fuel", "ev", "dog", "all", "none"}},
		{"prefer never filters", Preferences{Fuel: Prefer, EV: Prefer, DogWalk: Prefer}, []string{"fuel", "ev", "dog", "all", "none"}},
		{"require fuel", Preferences{Fuel: Require}, []string{"fuel", "all"}},
		{"require ev and dog walk", Preferences{EV: Require, DogWalk: Require}, []string{"all"}},
		{"require ev, prefer fuel", Preferences{EV: Require, Fuel: Prefer}, []string{"ev", "all"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slugs(Filter(services, tt.prefs)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRankKeepsJourneyOrder(t *testing.T) {
	in := []RouteService{svc("first", 1, nothing), svc("second", 5, nothing), svc("third", 3, nothing)}
	got := Rank(in, Preferences{})
	if want := []string{"first", "second", "third"}; !reflect.DeepEqual(slugs(got), want) {
		t.Errorf("order = %v, want %v (unchanged)", slugs(got), want)
	}
	if want := map[string]int{"second": 1, "third": 2, "first": 3}; !reflect.DeepEqual(ranks(got), want) {
		t.Errorf("ranks = %v, want %v", ranks(got), want)
	}
	if in[0].Rank != 0 {
		t.Error("Rank modified its input")
	}
}

func TestRankOrder(t *testing.T) {
	tests := []struct {
		name     string
		prefs    Preferences
		services []RouteService
		want     map[string]int
	}{
		{
			name:  "nice-to-haves beat rating",
			prefs: Preferences{EV: Prefer},
			services: []RouteService{
				svc("five-star-no-ev", 5, nothing),
				svc("two-star-ev", 2, ev),
			},
			want: map[string]int{"two-star-ev": 1, "five-star-no-ev": 2},
		},
		{
			name:  "more nice-to-haves beat fewer",
			prefs: Preferences{EV: Prefer, DogWalk: Prefer, Fuel: Prefer},
			services: []RouteService{
				svc("one", 5, ev),
				svc("three", 1, all),
			},
			want: map[string]int{"three": 1, "one": 2},
		},
		{
			name:  "required facilities don't count as nice-to-haves",
			prefs: Preferences{EV: Require},
			services: []RouteService{
				svc("ev-low", 2, ev),
				svc("ev-high", 4, ev),
			},
			want: map[string]int{"ev-high": 1, "ev-low": 2},
		},
		{
			name:  "unrated sorts below rated",
			prefs: Preferences{},
			services: []RouteService{
				svc("unrated", 0, all),
				svc("one-star", 1, nothing),
			},
			want: map[string]int{"one-star": 1, "unrated": 2},
		},
		{
			name:  "breadth breaks rating ties",
			prefs: Preferences{},
			services: []RouteService{
				svc("sparse", 3, fuel),
				svc("broad", 3, all),
			},
			want: map[string]int{"broad": 1, "sparse": 2},
		},
		{
			name:  "full ties go to whichever comes first on the journey",
			prefs: Preferences{},
			services: []RouteService{
				svc("earlier", 3, fuel),
				svc("later", 3, fuel),
			},
			want: map[string]int{"earlier": 1, "later": 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ranks(Rank(tt.services, tt.prefs)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ranks = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRankReasons(t *testing.T) {
	f := models.Facilities{FuelBrands: []string{"Shell", "BP"}, EVCharging: true, DogFriendly: true}
	tests := []struct {
		name   string
		prefs  Preferences
		rating float32
		want   []string
	}{
		{"nothing asked, unrated", Preferences{}, 0, nil},
		{"nothing asked, rated", Preferences{}, 3.5, []string{"Rated 3.5"}},
		{"asked for facilities it has", Preferences{Fuel: Require, DogWalk: Prefer}, 4, []string{"Fuel: Shell, BP", "Dog walk", "Rated 4.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Rank([]RouteService{svc("s", tt.rating, f)}, tt.prefs)[0].Reasons
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("reasons = %q, want %q", got, tt.want)
			}
		})
	}

	// A facility asked for but missing gets no badge.
	got := Rank([]RouteService{svc("s", 0, ev)}, Preferences{Fuel: Prefer, EV: Prefer})[0].Reasons
	if want := []string{"EV charging"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reasons = %q, want %q", got, want)
	}
}

func TestFacilityBreadth(t *testing.T) {
	if n := facilityBreadth(models.Facilities{}); n != 0 {
		t.Errorf("empty = %d, want 0", n)
	}
	f := models.Facilities{CateringBrands: []string{"Costa", "Greggs", "KFC"}, HasATM: true}
	if n := facilityBreadth(f); n != 2 {
		t.Errorf("three caterers + ATM = %d, want 2 (kinds, not counts)", n)
	}
}
