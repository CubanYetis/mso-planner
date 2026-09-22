// cmd/scraper/main.go — MSO scraper entrypoint
//
// Usage:
//
//	MSO_DB_DSN="postgres://mso:password@localhost:5432/mso?sslmode=disable" \
//	MSO_CONTACT="you@example.com" ./scraper
//
// MSO_CONTACT goes in the User-Agent sent to MSO and Nominatim. NOMINATIM_URL
// overrides the geocoder endpoint.
//
// The scraper is intentionally slow (3s delay per request) to be polite to MSO.
// Expect a full run to take 2–4 hours for the complete site (~500+ service pages).
// Run it once, then re-run occasionally (weekly/monthly) to pick up updates.
//
// Attribution: all scraped data links back to motorwayservices.uk via the mso_url
// field stored in the database, fulfilling MSO's attribution requirement.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
	"github.com/BigRedS/mso-planner/internal/geo"
	"github.com/BigRedS/mso-planner/internal/models"
	"github.com/BigRedS/mso-planner/internal/scraper"
)

// main configures the database and geocoder, then runs the scraper.
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Println("Connecting to database... (Ctrl-C stops cleanly between pages)")
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("Database connection failed: %v", err)
	}
	defer pool.Close()

	log.Println("Running migrations...")
	if err := pool.Migrate(ctx); err != nil {
		log.Fatalf("Migration failed: %v", err)
	}

	start := time.Now()
	var count int

	log.Println("Starting MSO scrape. This takes around 40 minutes — the delays are intentional.")
	log.Println("Data sourced from motorwayservices.uk — please ensure your app links back.")

	geocoder := geo.NewGeocoder(geoCfg, pool)

	err = scraper.Run(ctx, geoCfg, geocoder, func(sa models.ServiceArea) error {
		if err := pool.UpsertServiceArea(ctx, sa); err != nil {
			// Log but don't abort — one bad page shouldn't stop the whole run
			log.Printf("Failed to store %s: %v", sa.Name, err)
			return nil
		}
		count++
		if count%10 == 0 {
			log.Printf("Stored %d services so far (%.1f min elapsed)...",
				count, time.Since(start).Minutes())
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("Scrape failed: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		log.Println("Stopped early: signal received. Progress so far is saved; re-run to continue.")
	}

	elapsed := time.Since(start)
	// A cancelled ctx must not stop the summary from printing after a clean
	// stop: give this one query its own short-lived, uncancelled context.
	statsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	total, withFuel, withEV, statsErr := pool.Stats(statsCtx)
	if statsErr != nil {
		log.Printf("Could not fetch stats: %v", statsErr)
	} else {
		fmt.Printf("\n── Scrape complete ──────────────────────────────\n")
		fmt.Printf("  Services scraped this run: %d\n", count)
		fmt.Printf("  Total in database:         %d\n", total)
		fmt.Printf("  With fuel:                 %d\n", withFuel)
		fmt.Printf("  With EV charging:          %d\n", withEV)
		fmt.Printf("  Elapsed:                   %s\n", elapsed.Round(time.Second))
		fmt.Printf("─────────────────────────────────────────────────\n")
	}
}
