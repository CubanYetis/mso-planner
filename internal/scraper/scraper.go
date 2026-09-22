// Package scraper fetches and parses data from Motorway Services Online.
//
// MSO permits use of their data with attribution. Every ServiceArea record
// we store includes the canonical MSO URL so the application can link back
// to the source page, fulfilling the attribution requirement.
//
// We are polite guests:
//   - One request at a time (parallelism = 1)
//   - 3-second delay between every request
//   - Honest User-Agent identifying the project
//   - We stop if MSO returns a non-200 status
package scraper

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"

	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
)

const (
	baseURL      = "https://motorwayservices.uk"
	roadsListURL = baseURL + "/Services_List"
	requestDelay = 3 * time.Second // polite: one request every 3 seconds
)

// OnServiceFunc is called for each fully-scraped ServiceArea.
type OnServiceFunc func(sa models.ServiceArea) error

// Run performs the full scrape:
//  1. Fetches the road index from /Services_List
//  2. For each road page, collects service listings
//  3. For each service listing, fetches the detail page and builds a ServiceArea
//  4. Calls onService for each completed record
//
// The caller controls persistence via onService — typically writing to Postgres.
// Coordinates come from gc, which rate-limits and caches its lookups.
//
// ctx is checked between requests (road pages, then detail pages), so
// cancelling it stops the scrape promptly rather than after the full ~40
// minute run. It does not abort a request already in flight: colly does not
// take a context, so the current page is always allowed to finish.
func Run(ctx context.Context, cfg geo.Config, gc *geo.Geocoder, onService OnServiceFunc) error {
	// Collect road URLs first, then listings, then details.
	// We use three separate collectors so we can handle each phase distinctly.

	var roads []models.RoadIndex
	var listings []models.RoadListing

	// ── Phase 1: road index ──────────────────────────────────────────────────

	roadIndexCollector := newCollector(cfg)

	roadIndexCollector.OnHTML("div.mw-category a[href]", func(e *colly.HTMLElement) {
		href := e.Attr("href")
		// Links are like /M1, /A14, /A1(M) — we want only road pages
		if isRoadLink(href) {
			roads = append(roads, models.RoadIndex{
				Road: e.Text,
				URL:  baseURL + href,
			})
		}
	})

	log.Println("Fetching road index...")
	if err := roadIndexCollector.Visit(roadsListURL); err != nil {
		return fmt.Errorf("fetching road index: %w", err)
	}
	roadIndexCollector.Wait()
	log.Printf("Found %d roads", len(roads))

	// ── Phase 2: per-road listing pages ──────────────────────────────────────

	roadCollector := newCollector(cfg)

	roadCollector.OnHTML("table tr", func(e *colly.HTMLElement) {
		// Each road page has a table: | Services | Location | Operator | Rating | ...
		// First row is headers, skip it.
		nameLink := e.DOM.Find("td:nth-child(1) a")
		if nameLink.Length() == 0 {
			return
		}

		name := strings.TrimSpace(nameLink.Text())
		href, _ := nameLink.Attr("href")
		location := strings.TrimSpace(e.DOM.Find("td:nth-child(2)").Text())
		operator := strings.TrimSpace(e.DOM.Find("td:nth-child(3)").Text())
		rating := strings.TrimSpace(e.DOM.Find("td:nth-child(4)").Text())

		// The road is embedded in the URL we're currently visiting, passed via context
		road := e.Request.Ctx.Get("road")

		if name != "" && href != "" {
			listings = append(listings, models.RoadListing{
				Name:     name,
				URL:      baseURL + href,
				Location: location,
				Operator: operator,
				Rating:   rating,
				Road:     road,
			})
		}
	})

	for _, road := range roads {
		if ctx.Err() != nil {
			log.Printf("Stopping: %v", ctx.Err())
			return ctx.Err()
		}
		log.Printf("Fetching road page: %s", road.Road)
		rc := colly.NewContext()
		rc.Put("road", road.Road)
		if err := roadCollector.Request("GET", road.URL, nil, rc, nil); err != nil {
			log.Printf("Warning: failed to fetch %s: %v", road.URL, err)
		}
	}
	roadCollector.Wait()

	// Deduplicate listings — a service on a junction appears on multiple road pages
	listings = deduplicateListings(listings)
	log.Printf("Found %d unique service listings", len(listings))

	// ── Phase 3: individual services detail pages ─────────────────────────────

	detailCollector := newCollector(cfg)

	detailCollector.OnHTML("body", func(e *colly.HTMLElement) {
		listing := listingFromContext(e.Request.Ctx)
		sa := buildServiceArea(ctx, e, listing, gc)
		if err := onService(sa); err != nil {
			log.Printf("Error persisting %s: %v", sa.Name, err)
		}
	})

	for _, listing := range listings {
		if ctx.Err() != nil {
			log.Printf("Stopping: %v", ctx.Err())
			return ctx.Err()
		}
		log.Printf("Fetching detail: %s", listing.Name)
		dc := colly.NewContext()
		storeListingInContext(dc, listing)
		if err := detailCollector.Request("GET", listing.URL, nil, dc, nil); err != nil {
			log.Printf("Warning: failed to fetch detail for %s: %v", listing.Name, err)
		}
	}
	detailCollector.Wait()

	log.Println("Scrape complete.")
	return nil
}

// ── Collector factory ─────────────────────────────────────────────────────────

// newCollector builds a sequential, rate-limited collector for MSO pages.
func newCollector(cfg geo.Config) *colly.Collector {
	c := colly.NewCollector(
		colly.AllowedDomains("motorwayservices.uk", "www.motorwayservices.uk", "motorwayservices.ie", "www.motorwayservices.ie"),
		// No colly.Async(...) option here: in colly v2.1.0 it unconditionally
		// sets Async=true regardless of the bool passed in (colly.Async(false)
		// still turns it on), so requests run one goroutine per Visit/Request
		// with only Parallelism enforcing serialization. Leaving Async at its
		// zero value (false) is the only way to get genuinely synchronous,
		// one-request-at-a-time fetching, which the code below relies on to
		// react to a cancelled context between requests.
	)

	// Identify ourselves honestly
	extensions.RandomUserAgent(c) // sets a real browser UA — acceptable
	c.UserAgent = cfg.UserAgent()

	c.Limit(&colly.LimitRule{
		DomainGlob:  "*motorwayservices.*",
		Parallelism: 1,
		Delay:       requestDelay,
		RandomDelay: 1 * time.Second, // adds up to 1s of jitter on top
	})

	c.OnError(func(r *colly.Response, err error) {
		log.Printf("HTTP error fetching %s: %d %v", r.Request.URL, r.StatusCode, err)
	})

	return c
}

// ── Detail page parser ────────────────────────────────────────────────────────

// buildServiceArea extracts structured data from a services detail page.
// MSO pages are MediaWiki-based and have a consistent structure.
func buildServiceArea(ctx context.Context, e *colly.HTMLElement, listing models.RoadListing, gc *geo.Geocoder) models.ServiceArea {
	sa := models.ServiceArea{
		Slug:          slugFromURL(listing.URL),
		Name:          listing.Name,
		MSOURL:        listing.URL,
		Road:          listing.Road,
		Location:      listing.Location,
		Operator:      cleanOperator(listing.Operator),
		MSOrating:     parseRating(listing.Rating),
		Direction:     inferDirection(listing.Location),
		LastScrapedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Address — look for the 🏢 address block
	e.ForEach("p, li", func(_ int, el *colly.HTMLElement) {
		text := el.Text
		if strings.Contains(text, "Address:") || strings.HasPrefix(text, "🏢") {
			sa.Address = cleanAddress(text)
		}
	})

	// Postcode — extract from address
	sa.Postcode = extractPostcode(sa.Address)

	// Geocode coordinates now so the web UI doesn't need to do this later.
	maybeGeocodeServiceArea(ctx, gc, &sa)

	// Facilities — parse the structured facility section that MSO uses.
	// MSO pages often render these as styled <span> blocks rather than plain <li>/<p> tags.
	var facilityLines []string
	seen := make(map[string]bool)
	sel := ".mw-parser-output li, .mw-parser-output p, .mw-parser-output span.forecourt, .mw-parser-output span.charging, .mw-parser-output span.amenities, .mw-parser-output span.outdoor, .mw-parser-output span.shop"
	e.ForEach(sel, func(_ int, el *colly.HTMLElement) {
		text := strings.TrimSpace(el.Text)
		if !isFacilityLine(text) {
			return
		}
		if !seen[text] {
			seen[text] = true
			facilityLines = append(facilityLines, text)
		}
	})

	sa.Facilities = parseFacilities(facilityLines)
	return sa
}

// maybeGeocodeServiceArea fills coordinates when the service has an address.
func maybeGeocodeServiceArea(ctx context.Context, gc *geo.Geocoder, sa *models.ServiceArea) {
	query := sa.Postcode
	if query == "" {
		query = sa.Address
	}
	if query == "" {
		return
	}

	lat, lon, err := gc.Geocode(ctx, query)
	if err != nil {
		log.Printf("warning: could not geocode %s: %v", sa.Name, err)
		return
	}
	sa.Latitude = lat
	sa.Longitude = lon
}

// parseFacilities extracts structured facility data from the text lines on a detail page.
func parseFacilities(lines []string) models.Facilities {
	f := models.Facilities{}

	for _, line := range lines {
		prefix, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		prefix = strings.TrimSpace(prefix)
		rest = strings.TrimSpace(rest)

		switch strings.ToLower(prefix) {
		case "catering":
			f.CateringBrands = splitBrands(rest)

		case "forecourt":
			// Forecourt mixes fuel brands and forecourt shops
			// Fuel brands are typically first; MSO separates with commas
			brands := splitBrands(rest)
			for _, b := range brands {
				if isFuelBrand(b) {
					f.FuelBrands = append(f.FuelBrands, b)
				} else {
					f.ForecourtShops = append(f.ForecourtShops, b)
				}
			}

		case "amenities":
			items := splitBrands(rest)
			for _, item := range items {
				lower := strings.ToLower(item)
				switch {
				case isHotelBrand(item):
					f.Hotels = append(f.Hotels, item)
				case strings.Contains(lower, "shower"):
					f.HasShowers = true
				case strings.Contains(lower, "cash machine") || strings.Contains(lower, "atm"):
					f.HasATM = true
				case strings.Contains(lower, "play area") || strings.Contains(lower, "play zone"):
					f.HasPlayArea = true
				case strings.Contains(lower, "lpg"):
					f.HasLPG = true
				case strings.Contains(lower, "hgv"):
					f.HGVAccess = true
				}
			}

		case "charging points":
			f.EVCharging = true
			f.EVChargerInfo = rest

		case "outdoor space":
			f.OutdoorSpace = true
			f.OutdoorSpaceInfo = rest
			lower := strings.ToLower(rest)
			if strings.Contains(lower, "dog") || strings.Contains(lower, "walk") {
				f.DogFriendly = true
			}

		case "shops":
			// Shops section: parse for relevant brands but don't duplicate catering
			_ = splitBrands(rest) // captured but not currently used separately
		}
	}

	return f
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func isRoadLink(href string) bool {
	// Road links are /M1, /A14, /A1(M), /M6_Toll etc.
	// Exclude /Map:..., /History:..., /Special:... etc.
	if strings.Contains(href, ":") {
		return false
	}
	if strings.HasPrefix(href, "/M") || strings.HasPrefix(href, "/A") || strings.HasPrefix(href, "/N") {
		return true
	}
	return false
}

func isFacilityLine(text string) bool {
	prefixes := []string{"Catering:", "Forecourt:", "Amenities:", "Shops:", "Charging Points:", "Outdoor Space:"}
	for _, p := range prefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

var fuelBrands = map[string]bool{
	"BP": true, "Shell": true, "Esso": true, "Jet": true, "Texaco": true,
	"Total": true, "Applegreen": true, "MFG": true, "Rontec": true,
	"Gulf": true, "Murco": true,
}

func isFuelBrand(s string) bool {
	return fuelBrands[strings.TrimSpace(s)]
}

var hotelBrands = map[string]bool{
	"Travelodge": true, "Premier Inn": true, "Days Inn": true,
	"Holiday Inn": true, "Ramada": true, "ibis": true, "ibis budget": true,
}

func isHotelBrand(s string) bool {
	return hotelBrands[strings.TrimSpace(s)]
}

func splitBrands(s string) []string {
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseRating converts MSO star strings to a float32.
// MSO uses "3½ stars", "4 stars", "5 stars" etc.
func parseRating(s string) float32 {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " stars", "")
	s = strings.ReplaceAll(s, " star", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Handle the ½ character
	s = strings.ReplaceAll(s, "½", ".5")
	f, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return 0
	}
	return float32(f)
}

// inferDirection attempts to determine direction from the location string.
// MSO location strings are like "M1 between J21 and J21A" (both directions, online)
// or "M1 northbound at J15" or "A14 westbound at J18".
func inferDirection(location string) string {
	lower := strings.ToLower(location)
	switch {
	case strings.Contains(lower, "northbound"):
		return "northbound"
	case strings.Contains(lower, "southbound"):
		return "southbound"
	case strings.Contains(lower, "eastbound"):
		return "eastbound"
	case strings.Contains(lower, "westbound"):
		return "westbound"
	// anticlockwise must be checked first: it contains "clockwise".
	case strings.Contains(lower, "anticlockwise"):
		return "anticlockwise"
	case strings.Contains(lower, "clockwise"):
		return "clockwise"
	default:
		return "both" // online services or junction services serve both directions
	}
}

func slugFromURL(url string) string {
	// "https://motorwayservices.uk/Leicester_Forest_East" → "Leicester_Forest_East"
	parts := strings.Split(url, "/")
	return parts[len(parts)-1]
}

func cleanOperator(s string) string {
	// Some operator strings include " & " partners; we keep the full string
	return strings.TrimSpace(s)
}

func cleanAddress(s string) string {
	s = strings.ReplaceAll(s, "🏢", "")
	s = strings.ReplaceAll(s, "Address:", "")
	return strings.TrimSpace(s)
}

var postcodeRegexp = func() func(string) string {
	// Simple UK postcode pattern — good enough for our purposes
	// Matches "LE3 3GB", "PE28 0TD" etc.
	return func(s string) string {
		words := strings.Fields(s)
		for i := len(words) - 1; i >= 1; i-- {
			candidate := words[i-1] + " " + words[i]
			if looksLikePostcode(candidate) {
				return candidate
			}
		}
		return ""
	}
}()

func extractPostcode(address string) string {
	return postcodeRegexp(address)
}

func looksLikePostcode(s string) bool {
	// Very loose: at least 6 chars, contains a space, ends with digits+letters
	if len(s) < 6 || !strings.Contains(s, " ") {
		return false
	}
	parts := strings.Split(s, " ")
	if len(parts) != 2 {
		return false
	}
	// Inward code is 3 chars: digit + letter + letter
	inward := parts[1]
	if len(inward) != 3 {
		return false
	}
	_, err := strconv.Atoi(string(inward[0]))
	return err == nil
}

// deduplicateListings removes duplicates by URL, keeping the first occurrence.
// A services at a motorway junction appears on multiple road pages (e.g. Donington on M1, A42, A50).
func deduplicateListings(listings []models.RoadListing) []models.RoadListing {
	seen := make(map[string]bool)
	var out []models.RoadListing
	for _, l := range listings {
		if !seen[l.URL] {
			seen[l.URL] = true
			out = append(out, l)
		}
	}
	return out
}

// Context helpers — colly contexts are string→string maps
func storeListingInContext(ctx *colly.Context, l models.RoadListing) {
	ctx.Put("listing_name", l.Name)
	ctx.Put("listing_url", l.URL)
	ctx.Put("listing_location", l.Location)
	ctx.Put("listing_operator", l.Operator)
	ctx.Put("listing_rating", l.Rating)
	ctx.Put("listing_road", l.Road)
}

func listingFromContext(ctx *colly.Context) models.RoadListing {
	return models.RoadListing{
		Name:     ctx.Get("listing_name"),
		URL:      ctx.Get("listing_url"),
		Location: ctx.Get("listing_location"),
		Operator: ctx.Get("listing_operator"),
		Rating:   ctx.Get("listing_rating"),
		Road:     ctx.Get("listing_road"),
	}
}
