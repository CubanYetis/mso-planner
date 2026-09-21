package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Point is a WGS84 coordinate. The JSON tags match what the web UI's map
// script expects.
type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Route is the parts of an OSRM route we use.
type Route struct {
	Geometry []Point
	Distance float64 // metres
	Duration float64 // seconds
	// StepNames are the road names of each step, in order, unfiltered. Callers
	// decide which of them are roads they care about.
	StepNames []string
}

// Router asks an OSRM server for driving routes.
type Router struct {
	baseURL   string
	userAgent string
	http      *http.Client
}

// NewRouter builds an OSRM client from cfg.
func NewRouter(cfg Config) *Router {
	return &Router{
		baseURL:   cfg.OSRMURL,
		userAgent: cfg.UserAgent(),
		http:      &http.Client{Timeout: 12 * time.Second},
	}
}

type osrmResponse struct {
	Code   string `json:"code"`
	Routes []struct {
		Distance float64 `json:"distance"`
		Duration float64 `json:"duration"`
		Geometry struct {
			Coordinates [][]float64 `json:"coordinates"`
		} `json:"geometry"`
		Legs []struct {
			Steps []struct {
				Name string `json:"name"`
			} `json:"steps"`
		} `json:"legs"`
	} `json:"routes"`
}

// Route returns the fastest driving route between two points.
func (r *Router) Route(ctx context.Context, from, to Point) (Route, error) {
	// OSRM wants lon,lat order.
	url := fmt.Sprintf("%s/route/v1/driving/%.6f,%.6f;%.6f,%.6f?overview=full&geometries=geojson&steps=true",
		r.baseURL, from.Lon, from.Lat, to.Lon, to.Lat)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Route{}, err
	}
	req.Header.Set("User-Agent", r.userAgent)

	res, err := r.http.Do(req)
	if err != nil {
		return Route{}, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return Route{}, fmt.Errorf("routing failed: %s", res.Status)
	}

	var osrm osrmResponse
	if err := json.NewDecoder(res.Body).Decode(&osrm); err != nil {
		return Route{}, fmt.Errorf("decoding routing response: %w", err)
	}
	if osrm.Code != "Ok" || len(osrm.Routes) == 0 {
		return Route{}, fmt.Errorf("routing response invalid: code=%s", osrm.Code)
	}

	best := osrm.Routes[0]
	out := Route{
		Geometry: make([]Point, 0, len(best.Geometry.Coordinates)),
		Distance: best.Distance,
		Duration: best.Duration,
	}
	for _, c := range best.Geometry.Coordinates {
		if len(c) == 2 {
			out.Geometry = append(out.Geometry, Point{Lat: c[1], Lon: c[0]})
		}
	}
	for _, leg := range best.Legs {
		for _, step := range leg.Steps {
			out.StepNames = append(out.StepNames, step.Name)
		}
	}
	return out, nil
}
