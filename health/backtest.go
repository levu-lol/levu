package health

import (
	"fmt"
	"time"
)

// Scenario is a market's observed history, plus the ground truth about what
// happened to it.
//
// The harness is built to ingest real observation series; the synthetic ones in
// scenarios.go exist so the engine's *logic* can be validated before real data
// is available. They cannot validate its *calibration* — no amount of synthetic
// data tells you whether 75 is the right listing threshold for Robinhood Chain.
type Scenario struct {
	Name        string
	Description string
	Obs         []Observation
	// CollapseAt marks when the market failed, zero if it never did.
	CollapseAt time.Time
	// Atomic marks a collapse with no warning — an LP pull in a single block.
	// No detector can lead an atomic collapse; only the OI cap bounds it.
	Atomic bool
	// ShouldList records what a careful human would have decided, so the
	// harness can report disagreements rather than just describing behaviour.
	ShouldList bool
}

// Result is what the engine did with a scenario.
type Result struct {
	Scenario     string
	Listed       bool
	ListedAt     time.Time
	PeakLeverage int64
	PeakOICap    int64
	Transitions  []Transition
	Final        State

	// StoppedOpeningAt is when new exposure was last cut off, if ever.
	StoppedOpeningAt time.Time
	// LeadTime is how long before the collapse that happened. Negative means
	// the engine was still open for business when the market failed.
	LeadTime time.Duration
	// ExposureAtCollapse is the OI cap still standing when the market failed:
	// the actual loss surface, and the number that matters for an atomic rug.
	ExposureAtCollapse int64

	Verdict string
}

// Run drives the engine over a scenario.
func Run(s Scenario, cfg Config) Result {
	m := NewMachine(cfg)
	r := Result{Scenario: s.Name}

	openAt := time.Time{}
	exposureAt := int64(0)

	for _, o := range s.Obs {
		before := m.State()
		if t := m.Step(o, o.Time); t != nil {
			r.Transitions = append(r.Transitions, *t)
			if t.To == Live && !r.Listed {
				r.Listed, r.ListedAt = true, t.At
			}
			// Track the moment new exposure stopped being available.
			openingBefore := before == Live || before == Degraded
			openingAfter := t.To == Live || t.To == Degraded
			if openingBefore && !openingAfter {
				openAt = t.At
			}
		}
		c := m.Capacity()
		if c.MaxLeverage > r.PeakLeverage {
			r.PeakLeverage = c.MaxLeverage
		}
		if c.OICap > r.PeakOICap {
			r.PeakOICap = c.OICap
		}
		// Snapshot the standing exposure as of the collapse.
		if !s.CollapseAt.IsZero() && !o.Time.After(s.CollapseAt) {
			st := m.State()
			if st == Live || st == Degraded {
				exposureAt = c.OICap
			} else {
				exposureAt = 0
			}
		}
	}

	r.Final = m.State()
	r.StoppedOpeningAt = openAt
	r.ExposureAtCollapse = exposureAt
	if !s.CollapseAt.IsZero() && !openAt.IsZero() {
		r.LeadTime = s.CollapseAt.Sub(openAt)
	}
	r.Verdict = verdict(s, r)
	return r
}

func verdict(s Scenario, r Result) string {
	switch {
	case !s.ShouldList && r.Listed:
		return "MISS: listed a market that should never have been listed"
	case s.ShouldList && !r.Listed:
		return "MISS: refused a market that should have been listed"
	case !s.ShouldList && !r.Listed:
		return "correctly refused"
	case s.CollapseAt.IsZero():
		return "correctly listed, no collapse"
	case r.ExposureAtCollapse == 0:
		return fmt.Sprintf("closed to new exposure %v before collapse", r.LeadTime)
	case s.Atomic:
		return fmt.Sprintf("atomic collapse: undetectable, %d exposure capped", r.ExposureAtCollapse)
	default:
		return fmt.Sprintf("STILL OPEN at collapse with %d exposure", r.ExposureAtCollapse)
	}
}

// RunAll executes every scenario and returns the results in order.
func RunAll(scenarios []Scenario, cfg Config) []Result {
	out := make([]Result, 0, len(scenarios))
	for _, s := range scenarios {
		out = append(out, Run(s, cfg))
	}
	return out
}
