package planner

// Preferences is what the traveller cares about on one journey. Every
// planning decision that depends on the user reads it from here, passed in
// explicitly, never from request or session state: that keeps the planner
// testable with plain values, and lets something other than a web form (a
// saved profile, a CLI's flags) fill it in later.
//
// The zero value means "no preferences": nothing required.
type Preferences struct {
	RequireFuel bool // only show services selling fuel
	RequireEV   bool // only show services with EV charging
}
