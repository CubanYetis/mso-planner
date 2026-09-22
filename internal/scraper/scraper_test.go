package scraper

import (
	"reflect"
	"testing"

	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
)

func TestParseRating(t *testing.T) {
	tests := []struct {
		in   string
		want float32
	}{
		{"4 stars", 4},
		{"3½ stars", 3.5},
		{"1 star", 1},
		{"5 Stars", 5},
		{"", 0},
		{"unrated", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseRating(tt.in); got != tt.want {
				t.Errorf("parseRating(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestInferDirection(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"M1 northbound at J15", "northbound"},
		{"M1 Southbound at J15", "southbound"},
		{"A14 westbound at J18", "westbound"},
		{"A14 eastbound at J18", "eastbound"},
		{"M25 clockwise at J10", "clockwise"},
		{"M25 anticlockwise at J10", "anticlockwise"},
		{"M1 between J21 and J21A", "both"},
		{"", "both"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := inferDirection(tt.in); got != tt.want {
				t.Errorf("inferDirection(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitBrands(t *testing.T) {
	got := splitBrands(" Costa Coffee, Burger King ,, Greggs ")
	want := []string{"Costa Coffee", "Burger King", "Greggs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := splitBrands(""); len(got) != 0 {
		t.Errorf("empty input gave %q, want none", got)
	}
}

func TestParseFacilities(t *testing.T) {
	lines := []string{
		"Catering: Costa Coffee, Burger King, Greggs",
		"Forecourt: Shell, M&S Food, BP",
		"Amenities: Travelodge, Showers, Cash machine, Play area, LPG, HGV parking",
		"Charging Points: Shell Recharge 175kW CCS",
		"Outdoor Space: Grass verges; dog walk area",
		"not a facility line",
	}
	got := parseFacilities(lines)

	want := models.Facilities{
		CateringBrands:   []string{"Costa Coffee", "Burger King", "Greggs"},
		FuelBrands:       []string{"Shell", "BP"},
		ForecourtShops:   []string{"M&S Food"},
		Hotels:           []string{"Travelodge"},
		HasShowers:       true,
		HasATM:           true,
		HasPlayArea:      true,
		HasLPG:           true,
		HGVAccess:        true,
		EVCharging:       true,
		EVChargerInfo:    "Shell Recharge 175kW CCS",
		OutdoorSpace:     true,
		OutdoorSpaceInfo: "Grass verges; dog walk area",
		DogFriendly:      true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFacilities mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestParseFacilitiesEmpty(t *testing.T) {
	got := parseFacilities(nil)
	if !reflect.DeepEqual(got, models.Facilities{}) {
		t.Errorf("got %+v, want zero value", got)
	}
}

func TestSlugFromURL(t *testing.T) {
	got := slugFromURL("https://motorwayservices.uk/Leicester_Forest_East")
	if got != "Leicester_Forest_East" {
		t.Errorf("got %q", got)
	}
}

func TestExtractPostcode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Leicester Forest East, Leicester, LE3 3GB", "LE3 3GB"},
		{"Fen Road, Peterborough PE28 0TD", "PE28 0TD"},
		{"No postcode here", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := extractPostcode(tt.in); got != tt.want {
				t.Errorf("extractPostcode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsRoadLink(t *testing.T) {
	tests := []struct {
		href string
		want bool
	}{
		{"/M1", true},
		{"/A14", true},
		{"/A1(M)", true},
		{"/M6_Toll", true},
		{"/Map:Something", false},
		{"/Special:Search", false},
		{"/Leicester_Forest_East", false},
	}
	for _, tt := range tests {
		t.Run(tt.href, func(t *testing.T) {
			if got := isRoadLink(tt.href); got != tt.want {
				t.Errorf("isRoadLink(%q) = %v, want %v", tt.href, got, tt.want)
			}
		})
	}
}

func TestDeduplicateListings(t *testing.T) {
	in := []models.RoadListing{
		{URL: "https://motorwayservices.uk/Donington", Road: "M1"},
		{URL: "https://motorwayservices.uk/Other", Road: "M1"},
		{URL: "https://motorwayservices.uk/Donington", Road: "A42"},
	}
	got := deduplicateListings(in)
	want := []models.RoadListing{in[0], in[1]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v (first Donington kept, order preserved)", got, want)
	}
}

// newCollector must stay genuinely synchronous. colly v2.1.0's Async(...)
// option ignores the bool passed to it and always sets Async=true, so the
// fix is to not call it at all; asserting the zero value here catches that
// option being added back by mistake.
func TestNewCollectorIsSynchronous(t *testing.T) {
	cfg := geo.Config{Contact: "test@example.com"}
	if c := newCollector(cfg); c.Async {
		t.Error("newCollector's Collector.Async is true, want false (see the comment on the call in newCollector)")
	}
}
