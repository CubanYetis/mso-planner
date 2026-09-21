package geo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
)

func testConfig(url string) Config {
	return Config{NominatimURL: url, OSRMURL: url, Contact: "test@example.com"}
}

// stub starts a server that counts requests and answers with handler.
func stub(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, `[{"lat":"52.5","lon":"-1.25"}]`)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("MSO_CONTACT", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Error("want an error when MSO_CONTACT is unset")
	}

	t.Setenv("MSO_CONTACT", "me@example.com")
	t.Setenv("NOMINATIM_URL", "http://localhost:8088/")
	t.Setenv("OSRM_URL", "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NominatimURL != "http://localhost:8088" {
		t.Errorf("NominatimURL = %q, want trailing slash trimmed", cfg.NominatimURL)
	}
	if cfg.OSRMURL != DefaultOSRMURL {
		t.Errorf("OSRMURL = %q, want default", cfg.OSRMURL)
	}
	if !strings.Contains(cfg.UserAgent(), "me@example.com") {
		t.Errorf("UserAgent %q lacks the contact", cfg.UserAgent())
	}
}

func TestGeocodeSuccess(t *testing.T) {
	var got *http.Request
	srv, _ := stub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		okHandler(w, r)
	})
	g := NewGeocoder(testConfig(srv.URL), nil)

	lat, lon, err := g.Geocode(context.Background(), "LE3 3GB")
	if err != nil {
		t.Fatal(err)
	}
	if lat != 52.5 || lon != -1.25 {
		t.Errorf("got (%v, %v), want (52.5, -1.25)", lat, lon)
	}
	q := got.URL.Query()
	if q.Get("q") != "LE3 3GB" || q.Get("format") != "json" || q.Get("limit") != "1" || q.Get("countrycodes") != "gb,ie" {
		t.Errorf("unexpected query: %v", q)
	}
	if !strings.Contains(got.Header.Get("User-Agent"), "test@example.com") {
		t.Errorf("User-Agent = %q, want it to carry the contact", got.Header.Get("User-Agent"))
	}
}

func TestGeocodeErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr error // nil means any error
	}{
		{"no results", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[]`) }, ErrNoResults},
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `not json`) }, nil},
		{"bad latitude", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[{"lat":"x","lon":"1"}]`) }, nil},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, n := stub(t, tt.handler)
			g := NewGeocoder(testConfig(srv.URL), nil)

			_, _, err := g.Geocode(context.Background(), "somewhere")
			if err == nil {
				t.Fatal("want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if n.Load() != 1 {
				t.Errorf("%d requests, want 1 (only 429 is retried)", n.Load())
			}
		})
	}
}

func TestGeocodeEmptyQueryMakesNoRequest(t *testing.T) {
	srv, n := stub(t, okHandler)
	g := NewGeocoder(testConfig(srv.URL), nil)

	if _, _, err := g.Geocode(context.Background(), "   "); !errors.Is(err, ErrNoResults) {
		t.Errorf("err = %v, want ErrNoResults", err)
	}
	if n.Load() != 0 {
		t.Errorf("%d requests, want 0", n.Load())
	}
}

func TestGeocodeRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv, n := stub(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		okHandler(w, r)
	})
	g := NewGeocoder(testConfig(srv.URL), nil)

	if _, _, err := g.Geocode(context.Background(), "somewhere"); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Errorf("%d requests, want 2", n.Load())
	}
}

func TestGeocodeGivesUpAfterRepeated429(t *testing.T) {
	srv, n := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	g := NewGeocoder(testConfig(srv.URL), nil)

	_, _, err := g.Geocode(context.Background(), "somewhere")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want a 429 error", err)
	}
	if n.Load() != maxAttempts {
		t.Errorf("%d requests, want %d", n.Load(), maxAttempts)
	}
}

func TestGeocodeDoesNotObeyVeryLongRetryAfter(t *testing.T) {
	srv, n := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	g := NewGeocoder(testConfig(srv.URL), nil)

	start := time.Now()
	if _, _, err := g.Geocode(context.Background(), "somewhere"); err == nil {
		t.Fatal("want an error")
	}
	if n.Load() != 1 || time.Since(start) > time.Second {
		t.Errorf("%d requests in %s, want 1 request and no sleeping", n.Load(), time.Since(start))
	}
}

func TestGeocodeSpacesRequests(t *testing.T) {
	srv, _ := stub(t, okHandler)
	cfg := testConfig(srv.URL)
	cfg.MinInterval = 60 * time.Millisecond
	g := NewGeocoder(cfg, nil)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, _, err := g.Geocode(context.Background(), fmt.Sprintf("place %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// The first request is immediate; the next two each wait a full interval.
	if elapsed := time.Since(start); elapsed < 110*time.Millisecond {
		t.Errorf("3 requests took %s, want >= 120ms of spacing", elapsed)
	}
}

func TestGeocodeStopsWhenContextCancelled(t *testing.T) {
	srv, _ := stub(t, okHandler)
	cfg := testConfig(srv.URL)
	cfg.MinInterval = time.Hour
	g := NewGeocoder(cfg, nil)

	// The first call takes the free slot; the second would wait an hour.
	if _, _, err := g.Geocode(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := g.Geocode(ctx, "two"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestGeocodeUsesCache(t *testing.T) {
	dsn := os.Getenv("MSO_TEST_DSN")
	if dsn == "" {
		t.Skip("MSO_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	srv, n := stub(t, okHandler)
	g := NewGeocoder(testConfig(srv.URL), pool)
	place := fmt.Sprintf("Cache Test %d", time.Now().UnixNano())

	for _, q := range []string{place, place, "  " + strings.ToUpper(place) + "  "} {
		lat, lon, err := g.Geocode(ctx, q)
		if err != nil || lat != 52.5 || lon != -1.25 {
			t.Fatalf("Geocode(%q) = (%v, %v, %v)", q, lat, lon, err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("%d upstream requests, want 1 (rest served from cache, case/space-insensitively)", n.Load())
	}
}

const osrmOK = `{"code":"Ok","routes":[{"distance":1500.5,"duration":90,
 "geometry":{"coordinates":[[-1.0,52.0],[-1.1,52.1],[5]]},
 "legs":[{"steps":[{"name":"M1"},{"name":""},{"name":"A14"}]},{"steps":[{"name":"M6"}]}]}]}`

func TestRouterRoute(t *testing.T) {
	var got *http.Request
	srv, _ := stub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprint(w, osrmOK)
	})

	r, err := NewRouter(testConfig(srv.URL)).Route(context.Background(), Point{Lat: 52, Lon: -1}, Point{Lat: 53, Lon: -2})
	if err != nil {
		t.Fatal(err)
	}

	if want := "/route/v1/driving/-1.000000,52.000000;-2.000000,53.000000"; got.URL.Path != want {
		t.Errorf("path = %q, want %q (OSRM wants lon,lat)", got.URL.Path, want)
	}
	if !strings.Contains(got.Header.Get("User-Agent"), "test@example.com") {
		t.Errorf("User-Agent = %q", got.Header.Get("User-Agent"))
	}
	if r.Distance != 1500.5 || r.Duration != 90 {
		t.Errorf("distance/duration = %v/%v", r.Distance, r.Duration)
	}
	wantPts := []Point{{Lat: 52, Lon: -1}, {Lat: 52.1, Lon: -1.1}}
	if len(r.Geometry) != 2 || r.Geometry[0] != wantPts[0] || r.Geometry[1] != wantPts[1] {
		t.Errorf("geometry = %+v, want %+v (malformed coordinate skipped, GeoJSON lon,lat flipped)", r.Geometry, wantPts)
	}
	if got, want := strings.Join(r.StepNames, ","), "M1,,A14,M6"; got != want {
		t.Errorf("step names = %q, want %q", got, want)
	}
}

func TestRouterErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }},
		{"osrm code not Ok", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"code":"NoRoute","routes":[]}`) }},
		{"no routes", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"code":"Ok","routes":[]}`) }},
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `<html>`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := stub(t, tt.handler)
			if _, err := NewRouter(testConfig(srv.URL)).Route(context.Background(), Point{}, Point{}); err == nil {
				t.Error("want an error")
			}
		})
	}
}
