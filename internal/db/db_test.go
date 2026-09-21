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
