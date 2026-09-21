// Package geo wraps the two external services we depend on: Nominatim for
// geocoding and OSRM for routing.
//
// The Nominatim client enforces its usage policy (one request per second, a
// real User-Agent), backs off when told to, and caches results in Postgres so
// repeat lookups never leave the machine.
package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BigRedS/mso-planner/internal/db"
)

const (
	maxAttempts = 3

	// Longest we will sleep on a 429. The web UI calls this while a user waits,
	// so a very long Retry-After is better reported as an error than obeyed.
	maxRetryWait = 10 * time.Second
)

// ErrNoResults means Nominatim understood the request but found nothing.
var ErrNoResults = errors.New("no geocoding results")

// Geocoder looks up coordinates for free-text queries. It is safe for
// concurrent use; all callers share one rate limiter.
type Geocoder struct {
	baseURL     string
	userAgent   string
	minInterval time.Duration
	http        *http.Client
	cache       *db.Pool // nil disables caching

	mu   sync.Mutex
	last time.Time // when the last upstream request was allowed to start
}

// NewGeocoder builds a Geocoder. cache may be nil to disable caching.
func NewGeocoder(cfg Config, cache *db.Pool) *Geocoder {
	return &Geocoder{
		baseURL:     cfg.NominatimURL,
		userAgent:   cfg.UserAgent(),
		minInterval: cfg.MinInterval,
		http:        &http.Client{Timeout: 12 * time.Second},
		cache:       cache,
	}
}

type nominatimResult struct {
	Lat string `json:"lat"`
	Lon string `json:"lon"`
}

// Geocode returns the latitude and longitude for query, from the cache if
// possible. Cache failures are not fatal: they only cost an upstream request.
func (g *Geocoder) Geocode(ctx context.Context, query string) (lat, lon float64, err error) {
	key := cacheKey(query)
	if key == "" {
		return 0, 0, ErrNoResults
	}

	if g.cache != nil {
		if lat, lon, ok, err := g.cache.GetGeocode(ctx, key); err == nil && ok {
			return lat, lon, nil
		}
	}

	lat, lon, err = g.fetch(ctx, query)
	if err != nil {
		return 0, 0, err
	}

	if g.cache != nil {
		// Best effort: the lookup succeeded, so don't fail the caller over the cache.
		_ = g.cache.PutGeocode(ctx, key, lat, lon)
	}
	return lat, lon, nil
}

// cacheKey normalises a query so "le3  3gb" and "LE3 3GB" share an entry.
func cacheKey(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

func (g *Geocoder) fetch(ctx context.Context, query string) (float64, float64, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := g.waitTurn(ctx); err != nil {
			return 0, 0, err
		}

		lat, lon, retryAfter, err := g.fetchOnce(ctx, query)
		if err == nil {
			return lat, lon, nil
		}
		lastErr = err
		if retryAfter < 0 || attempt == maxAttempts {
			// Not a 429, or out of attempts.
			break
		}
		if retryAfter > maxRetryWait {
			return 0, 0, fmt.Errorf("%w (asked us to wait %s, too long)", err, retryAfter)
		}
		if err := sleep(ctx, retryAfter); err != nil {
			return 0, 0, err
		}
	}
	return 0, 0, lastErr
}

// fetchOnce makes a single upstream request. retryAfter is negative unless the
// server answered 429, in which case it is how long to wait before retrying.
func (g *Geocoder) fetchOnce(ctx context.Context, query string) (lat, lon float64, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/search", nil)
	if err != nil {
		return 0, 0, -1, err
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("limit", "1")
	q.Set("countrycodes", "gb,ie")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", g.userAgent)

	res, err := g.http.Do(req)
	if err != nil {
		return 0, 0, -1, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusTooManyRequests {
		return 0, 0, parseRetryAfter(res.Header.Get("Retry-After")), fmt.Errorf("geocoding failed: %s", res.Status)
	}
	if res.StatusCode != http.StatusOK {
		return 0, 0, -1, fmt.Errorf("geocoding failed: %s", res.Status)
	}

	var results []nominatimResult
	if err := json.NewDecoder(res.Body).Decode(&results); err != nil {
		return 0, 0, -1, fmt.Errorf("decoding geocoding response: %w", err)
	}
	if len(results) == 0 {
		return 0, 0, -1, ErrNoResults
	}

	lat, err = strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return 0, 0, -1, err
	}
	lon, err = strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return 0, 0, -1, err
	}
	return lat, lon, -1, nil
}

// waitTurn blocks until at least minInterval has passed since the previous
// upstream request, so concurrent callers queue rather than burst. The slot is
// reserved before sleeping, which keeps the spacing right under contention.
func (g *Geocoder) waitTurn(ctx context.Context) error {
	for {
		g.mu.Lock()
		now := time.Now()
		start := g.last.Add(g.minInterval)
		if start.Before(now) {
			start = now
		}
		g.mu.Unlock()

		if err := sleep(ctx, time.Until(start)); err != nil {
			return err
		}

		g.mu.Lock()
		now = time.Now()
		if g.last.Add(g.minInterval).After(now) {
			g.mu.Unlock()
			continue
		}
		g.last = now
		g.mu.Unlock()
		return nil
	}
}

// parseRetryAfter reads the delay-seconds form of Retry-After. Nominatim does
// not always send it, so a missing or unparseable header falls back to 2s.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 2 * time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
