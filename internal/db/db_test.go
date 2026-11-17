package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
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

// TestMigrateConcurrent is a regression test: several processes (web and
// scraper deploying together, or test packages run in parallel) migrating an
// empty database at once used to fail with "duplicate key value violates
// unique constraint pg_type_typname_nsp_index", because CREATE TABLE IF NOT
// EXISTS is not safe against a concurrent create of the same table.
//
// The race only happens when the tables don't exist yet, so each run migrates
// into a fresh, empty schema (via search_path) rather than the shared one.
func TestMigrateConcurrent(t *testing.T) {
	ctx := context.Background()
	dsn := freshSchemaDSN(t)

	// One pool per goroutine, standing in for separate processes.
	const n = 8
	pools := make([]*Pool, n)
	for i := range pools {
		p, err := Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		pools[i] = p
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, p := range pools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.Migrate(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
}

// TestMigrateWhileWriting is a regression test: Migrate against an existing
// schema while another session inserts into service_roads used to deadlock.
// ALTER TABLE ... ADD COLUMN IF NOT EXISTS takes an AccessExclusive lock on
// service_areas even when the column exists, and with the whole schema in one
// transaction Migrate then held it while waiting to lock service_roads; the
// insert holds service_roads and waits on service_areas for its foreign-key
// check. That's the web pod restarting mid-scrape, or test packages run in
// parallel.
func TestMigrateWhileWriting(t *testing.T) {
	ctx := context.Background()
	dsn := freshSchemaDSN(t)
	migrator, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(migrator.Close)
	writer, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	if err := migrator.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var id int64
	if err := writer.pool.QueryRow(ctx, `
		INSERT INTO service_areas (slug, name, mso_url, road) VALUES ('w', 'w', 'w', 'M1') RETURNING id
	`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// Race the two for a while; the deadlock needs an unlucky interleaving.
	deadline := time.Now().Add(3 * time.Second)
	writeErr := make(chan error, 1)
	go func() {
		for i := 0; time.Now().Before(deadline); i++ {
			if _, err := writer.pool.Exec(ctx,
				`INSERT INTO service_roads (service_area_id, road) VALUES ($1, $2)`,
				id, fmt.Sprintf("R%d", i)); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()
	for time.Now().Before(deadline) {
		if err := migrator.Migrate(ctx); err != nil {
			t.Errorf("Migrate while writing: %v", err)
			break
		}
	}
	if err := <-writeErr; err != nil {
		t.Errorf("insert while migrating: %v", err)
	}
}

// freshSchemaDSN creates an empty schema, dropped when the test ends, and
// returns MSO_TEST_DSN pointed at it via search_path. For tests whose race
// needs tables that are missing, or that nothing else is using.
func freshSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MSO_TEST_DSN")
	if dsn == "" {
		t.Skip("MSO_TEST_DSN not set")
	}
	ctx := context.Background()
	admin := testPool(t)
	schemaName := fmt.Sprintf("migrate_test_%d", time.Now().UnixNano())
	if _, err := admin.pool.Exec(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.pool.Exec(ctx, "DROP SCHEMA "+schemaName+" CASCADE")
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schemaName)
	u.RawQuery = q.Encode()
	return u.String()
}
