package planner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BigRedS/mso-planner/internal/models"
)

// check is one facility the traveller might want, checked against one
// service: how much they want it, whether the service has it, and how to
// say so in a badge.
type check struct {
	want  Want
	has   bool
	badge string
}

// checklist lists every facility Preferences can ask about, for one
// service. Adding a facility means a field on Preferences and a line here.
func checklist(f models.Facilities, prefs Preferences) []check {
	return []check{
		{prefs.Fuel, len(f.FuelBrands) > 0, "Fuel: " + strings.Join(f.FuelBrands, ", ")},
		{prefs.EV, f.EVCharging, "EV charging"},
		{prefs.DogWalk, f.DogFriendly, "Dog walk"},
	}
}

// Filter drops the services missing anything the traveller requires. Order
// is preserved.
func Filter(services []RouteService, prefs Preferences) []RouteService {
	var out []RouteService
	for _, s := range services {
		if meetsRequirements(s.Facilities, prefs) {
			out = append(out, s)
		}
	}
	return out
}

func meetsRequirements(f models.Facilities, prefs Preferences) bool {
	for _, c := range checklist(f, prefs) {
		if c.want == Require && !c.has {
			return false
		}
	}
	return true
}

// Rank sets Rank (1 = best) and Reasons on every service, without
// reordering them: callers keep journey order and show the rank alongside.
//
// Best means, in order:
//  1. meets more of the traveller's nice-to-haves
//  2. higher MSO rating (unrated, 0, sorts last)
//  3. more kinds of facility (see facilityBreadth)
//  4. earlier on the journey, so ranks are always distinct and stable
//
// Deliberately simple; it has to stay explainable, which is what Reasons
// is for.
func Rank(services []RouteService, prefs Preferences) []RouteService {
	out := make([]RouteService, len(services))
	copy(out, services)

	type key struct {
		preferred int
		rating    float32
		breadth   int
		index     int
	}
	keys := make([]key, len(out))
	for i := range out {
		out[i].Reasons = reasons(out[i].ServiceArea, prefs)
		keys[i] = key{
			preferred: preferredMet(out[i].Facilities, prefs),
			rating:    out[i].MSOrating,
			breadth:   facilityBreadth(out[i].Facilities),
			index:     i,
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		ka, kb := keys[a], keys[b]
		if ka.preferred != kb.preferred {
			return ka.preferred > kb.preferred
		}
		if ka.rating != kb.rating {
			return ka.rating > kb.rating
		}
		if ka.breadth != kb.breadth {
			return ka.breadth > kb.breadth
		}
		return ka.index < kb.index
	})
	for rank, k := range keys {
		out[k.index].Rank = rank + 1
	}
	return out
}

func preferredMet(f models.Facilities, prefs Preferences) int {
	n := 0
	for _, c := range checklist(f, prefs) {
		if c.want == Prefer && c.has {
			n++
		}
	}
	return n
}

// reasons explains a service's place in the results: each facility the
// traveller asked for (required or preferred) that it has, then its rating.
func reasons(sa models.ServiceArea, prefs Preferences) []string {
	var out []string
	for _, c := range checklist(sa.Facilities, prefs) {
		if c.want != DontCare && c.has {
			out = append(out, c.badge)
		}
	}
	if sa.MSOrating > 0 {
		out = append(out, fmt.Sprintf("Rated %.1f", sa.MSOrating))
	}
	return out
}

// facilityBreadth counts the kinds of facility a service has, not how many
// of each: three coffee chains count once. A crude stand-in for "how much is
// there", used only to break ties.
func facilityBreadth(f models.Facilities) int {
	n := 0
	for _, has := range []bool{
		len(f.CateringBrands) > 0,
		len(f.FuelBrands) > 0,
		len(f.ForecourtShops) > 0,
		len(f.Hotels) > 0,
		f.HasShowers,
		f.HasATM,
		f.HasPlayArea,
		f.HasLPG,
		f.EVCharging,
		f.OutdoorSpace,
		f.DogFriendly,
	} {
		if has {
			n++
		}
	}
	return n
}
