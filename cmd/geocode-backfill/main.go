// cmd/geocode-backfill — one-off command to geocode services that have no
// coordinates.
//
// Services scraped before the scraper geocoded at scrape time (or whose
// geocoding failed) have no coordinates, and the web UI no longer geocodes in
// the request path — they would never appear in route results. Run this once
// after upgrading; it is safe to re-run, and results are shared with the
// geocode cache.
//
//	MSO_DB_DSN="postgres://mso:password@localhost:5432/mso?sslmode=disable" \
//	MSO_CONTACT="you@example.com" go run ./cmd/geocode-backfill
//
// It is deliberately slow (Nominatim's one request per second), like the
// scraper.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
	"github.com/BigRedS/mso-planner/internal/geo"
)

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	dsn := os.Getenv("MSO_DB_DSN")
	if dsn == "" {
		log.Fatal("MSO_DB_DSN environment variable is required")
	}
	geoCfg, err := geo.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("Database connection failed: %v", err)
	}
	defer pool.Close()

	pending, err := pool.ListServicesMissingCoordinates(ctx)
	if err != nil {
		log.Fatalf("Listing services: %v", err)
	}
	log.Printf("%d services missing coordinates", len(pending))

	geocoder := geo.NewGeocoder(geoCfg, pool)
	start := time.Now()
	var updated, failed int
	for _, sa := range pending {
		query := sa.Postcode
		if query == "" {
			query = sa.Address
		}
		if query == "" {
			log.Printf("%s: no postcode or address, skipping", sa.Slug)
			failed++
			continue
		}
		lat, lon, err := geocoder.Geocode(ctx, query)
		if err != nil {
			log.Printf("%s: geocoding %q failed: %v", sa.Slug, query, err)
			failed++
			continue
		}
		if err := pool.UpdateServiceCoordinates(ctx, sa.Slug, lat, lon); err != nil {
			log.Printf("%s: storing coordinates failed: %v", sa.Slug, err)
			failed++
			continue
		}
		updated++
	}
	log.Printf("Done: %d updated, %d failed, %d untouched in %s",
		updated, failed, len(pending)-updated-failed, time.Since(start).Round(time.Second))
}
