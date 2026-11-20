package planner

import (
	"regexp"
	"strings"

	"github.com/BigRedS/mso-planner/internal/geo"
)

// roadRefRegexp matches a whole road reference, not a substring: the road
// number, an optional "(M)" suffix and an optional "Toll". It must stay
// anchored so that "A1" and "A1(M)", which are different roads, never collapse.
var roadRefRegexp = regexp.MustCompile(`^([AMN]\d+)\s?(\(M\))?(\s?TOLL)?$`)

// ExtractRoadNames returns the unique motorway and A-road names in route order.
// Road numbers live in a step's Ref (possibly several, joined by ";"); Name is
// only consulted as a fallback because it is usually a street name.
func ExtractRoadNames(steps []geo.Step) []string {
	seen := make(map[string]bool)
	var roads []string

	for _, step := range steps {
		candidates := strings.Split(step.Ref, ";")
		candidates = append(candidates, step.Name)
		for _, c := range candidates {
			road := normalizeRoadName(c)
			if road == "" || seen[road] {
				continue
			}
			seen[road] = true
			roads = append(roads, road)
		}
	}

	return roads
}

// normalizeRoadName returns the canonical form of a road reference as stored in
// service_roads.road ("M1", "A1(M)", "M6 Toll"), or "" if name is not exactly
// a road reference.
func normalizeRoadName(name string) string {
	m := roadRefRegexp.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(name)))
	if m == nil {
		return ""
	}
	road := m[1]
	if m[2] != "" {
		road += "(M)"
	}
	if m[3] != "" {
		road += " Toll"
	}
	return road
}
