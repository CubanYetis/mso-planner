package planner

// Preferences is what the traveller cares about on one journey. Every
// planning decision that depends on the user reads it from here, passed in
// explicitly, never from request or session state: that keeps the planner
// testable with plain values, and lets something other than a web form (a
// saved profile, a CLI's flags) fill it in later.
//
// The zero value means "no preferences": nothing required or preferred.
type Preferences struct {
	Fuel    Want
	EV      Want
	DogWalk Want
}

// Want is how much the traveller cares about one facility.
type Want int

const (
	DontCare Want = iota
	Prefer        // nice to have: ranks stops that have it higher
	Require       // must have: stops without it are dropped
)

// String is the form/query-string spelling, also used by the template to
// mark the selected option.
func (w Want) String() string {
	switch w {
	case Prefer:
		return "prefer"
	case Require:
		return "require"
	default:
		return "any"
	}
}
