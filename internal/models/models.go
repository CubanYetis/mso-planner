package models

// ServiceArea represents a single services location as scraped from MSO.
// All fields are nullable where MSO may not have the information.
type ServiceArea struct {
	ID     int64
	Slug   string // e.g. "Leicester_Forest_East" — derived from MSO URL path
	Name   string // e.g. "Leicester Forest East"
	MSOURL string // full canonical URL on motorwayservices.uk — used for attribution links

	// Location
	Road      string // e.g. "M1", "A14", "A1(M)"
	Direction string // "northbound", "southbound", "both", "junction" — best-effort from location string
	Location  string // raw location string from MSO, e.g. "M1 between J21 and J21A"
	Postcode  string
	Address   string
	Latitude  float64
	Longitude float64

	// Operator & rating
	Operator  string  // e.g. "Welcome Break", "Moto", "Roadchef"
	MSOrating float32 // 0 if unrated; MSO uses 0.5-step stars up to 5

	// Facilities — these come from the detail page
	Facilities Facilities

	// Scraper bookkeeping
	LastScrapedAt string // RFC3339
}

// Facilities holds all the structured facility data we extract from a services detail page.
// Slices hold the brand/chain names exactly as MSO lists them.
type Facilities struct {
	// Catering — named chains in the "Catering:" section
	CateringBrands []string // e.g. ["Costa Coffee", "Burger King", "Greggs"]

	// Forecourt — fuel brands and forecourt shops
	FuelBrands     []string // e.g. ["Shell", "BP", "Esso"]
	ForecourtShops []string // e.g. ["M&S Food", "Wild Bean Café", "SPAR"]

	// Amenities — hotels, ATMs, showers etc
	Hotels      []string // e.g. ["Travelodge", "Premier Inn"]
	HasShowers  bool
	HasATM      bool
	HasPlayArea bool
	HasLPG      bool
	HGVAccess   bool

	// EV charging — parsed from "Charging Points:" line
	EVCharging    bool
	EVChargerInfo string // raw string e.g. "Shell Recharge 175kW CCS & 70kW CHAdeMO"

	// Outdoor space
	OutdoorSpace     bool
	OutdoorSpaceInfo string // raw string e.g. "Grass verges; dog walk area"
	DogFriendly      bool   // true if MSO mentions dog walk / dog area

	// Parking
	ParkingInfo string // raw parking notes; structured later if needed
}

// RoadIndex is the list of roads from the Services_List page.
type RoadIndex struct {
	Road string // e.g. "M1"
	URL  string // e.g. "https://motorwayservices.uk/M1"
}

// RoadListing is an entry from a per-road page table.
type RoadListing struct {
	Name     string
	URL      string
	Location string
	Operator string
	Rating   string // raw star string, e.g. "3½ stars"
	Road     string // which road page this came from
}
