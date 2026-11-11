package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// testPool connects to the Postgres named by MSO_TEST_DSN and skips the test
// if it isn't set. Use a throwaway database, not your real one:
//
//	docker compose exec postgres createdb -U mso mso_test
//	export MSO_TEST_DSN="postgres://mso:msopassword@localhost:5432/mso_test?sslmode=disable"
func testPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("MSO_TEST_DSN")
	if dsn == "" {
		t.Skip("MSO_TEST_DSN not set")
	}
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return p
}

// TestGeocodeCache verifies cache misses, writes, and subsequent reads.
func TestGeocodeCache(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	key := fmt.Sprintf("test query %d", time.Now().UnixNano())

	_, _, ok, err := p.GetGeocode(ctx, key)
	if err != nil || ok {
		t.Fatalf("miss: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := p.PutGeocode(ctx, key, 52.5, -1.25); err != nil {
		t.Fatal(err)
	}
	lat, lon, ok, err := p.GetGeocode(ctx, key)
	if err != nil || !ok || lat != 52.5 || lon != -1.25 {
		t.Fatalf("hit: got (%v, %v, %v, %v), want (52.5, -1.25, true, nil)", lat, lon, ok, err)
	}

	if err := p.PutGeocode(ctx, key, 1, 2); err != nil {
		t.Fatal(err)
	}
	lat, lon, _, _ = p.GetGeocode(ctx, key)
	if lat != 1 || lon != 2 {
		t.Errorf("overwrite: got (%v, %v), want (1, 2)", lat, lon)
	}
}

// TestListServicesNullColumns is a regression test: rows with NULLs in the
// optional text columns (possible via hand-maintained data; the scraper
// writes "") must not break ListServices.
func TestListServicesNullColumns(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	slug := fmt.Sprintf("null-columns-%d", time.Now().UnixNano())

	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO service_areas (slug, name, mso_url, road, direction, location_raw, postcode, address, operator, last_scraped_at)
		VALUES ($1, 'Null Test', 'https://example.com/'||$1, 'M1', 'both', NULL, NULL, NULL, NULL, NULL)
		RETURNING id
	`, slug).Scan(&id)
	if err != nil {
		t.Fatalf("inserting service: %v", err)
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO facilities (service_area_id, ev_charger_info, outdoor_space_info)
		VALUES ($1, NULL, NULL)
	`, id); err != nil {
		t.Fatalf("inserting facilities: %v", err)
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO service_roads (service_area_id, road) VALUES ($1, 'M1')
	`, id); err != nil {
		t.Fatalf("inserting service_roads: %v", err)
	}
	t.Cleanup(func() {
		p.pool.Exec(ctx, `DELETE FROM service_areas WHERE id = $1`, id)
	})

	services, err := p.ListServices(ctx, ListServicesOptions{Road: "M1"})
	if err != nil {
		t.Fatalf("ListServices with NULL columns: %v", err)
	}
	found := false
	for _, sa := range services {
		if sa.Slug == slug {
			found = true
		}
	}
	if !found {
		t.Error("inserted service not returned by ListServices")
	}
}

// TestListServicesMissingCoordinates checks that NULL and zero coordinates
// are both treated as missing, and real coordinates are not.
func TestListServicesMissingCoordinates(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("missing-coords-%d", time.Now().UnixNano())

	insert := func(slug string, lat, lon interface{}) {
		t.Helper()
		if _, err := p.pool.Exec(ctx, `
			INSERT INTO service_areas (slug, name, mso_url, road, direction, postcode, latitude, longitude)
			VALUES ($1, $1, 'https://example.com/'||$1, 'M1', 'both', 'LE1 1AA', $2, $3)
		`, slug, lat, lon); err != nil {
			t.Fatalf("inserting %s: %v", slug, err)
		}
	}
	insert(prefix+"-null", nil, nil)
	insert(prefix+"-zero", 0, 0)
	insert(prefix+"-geocoded", 52.5, -1.25)
	t.Cleanup(func() {
		p.pool.Exec(ctx, `DELETE FROM service_areas WHERE slug LIKE $1`, prefix+"%")
	})

	missing, err := p.ListServicesMissingCoordinates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sa := range missing {
		got[sa.Slug] = true
		if sa.Postcode != "LE1 1AA" {
			t.Errorf("%s: postcode = %q, want LE1 1AA", sa.Slug, sa.Postcode)
		}
	}
	if !got[prefix+"-null"] || !got[prefix+"-zero"] {
		t.Errorf("null and zero rows should be missing coordinates, got %v", got)
	}
	if got[prefix+"-geocoded"] {
		t.Error("geocoded row reported as missing coordinates")
	}
}
