package health

import (
	"fmt"
	"time"
)

// State is where a market sits in its lifecycle.
type State int

const (
	// Preparing: observed but not yet qualified. Spot may trade; no perp.
	Preparing State = iota
	// Live: perp trading at the tier the score supports.
	Live
	// Degraded: still trading, but at reduced leverage and a tighter cap.
	Degraded
	// SpotOnly: openable at 1x, fully collateralised. No leverage is offered,
	// but the market is not closed: positions can still be built and unwound
	// at the trader's own pace.
	//
	// This rung is what stops a market fading into a cliff. Without it a lane
	// whose score drifts below the lowest leverage tier goes straight from 2x
	// to accepting nothing, which forces every holder toward the exit at once
	// on a book that is already thinner than it was.
	SpotOnly
	// ReduceOnly: positions may close, none may open.
	ReduceOnly
	// Closed: settled and gone.
	Closed
)

func (s State) String() string {
	switch s {
	case Preparing:
		return "preparing"
	case Live:
		return "live"
	case Degraded:
		return "degraded"
	case SpotOnly:
		return "spot-only"
	case ReduceOnly:
		return "reduce-only"
	case Closed:
		return "closed"
	}
	return "unknown"
}

// Openable reports whether a market in this state may take on new exposure.
//
// One definition, because the set has grown: every place that enumerated
// "Live or Degraded" by hand was a place that would silently omit a new rung.
func (s State) Openable() bool {
	return s == Live || s == Degraded || s == SpotOnly
}

// spotReason names which condition withdrew the leverage, because "below the
// threshold" sends whoever reads it to the wrong number half the time.
func (m *Machine) spotReason(s Score, o Observation) string {
	if !s.Leverable() {
		return s.LeverageBlockers[0]
	}
	if m.cfg.MinBookForLeverage > 0 && o.BookDepthWithin2Pct < m.cfg.MinBookForLeverage {
		return fmt.Sprintf("resting depth %d is below the %d needed to lever against",
			o.BookDepthWithin2Pct, m.cfg.MinBookForLeverage)
	}
	return fmt.Sprintf("score %d below the spot-only threshold %d", s.Total, m.cfg.SpotOnlyBelow)
}

// Ceiling is the most leverage this state will allow, whatever the tier ladder
// proposes. The tier says what the score has earned; the state says what the
// market's condition permits, and the lower of the two wins.
func (s State) Ceiling() int64 {
	switch s {
	case Live:
		return 0 // no ceiling; the tier decides
	case Degraded:
		return 2
	case SpotOnly:
		return 1
	}
	return 0
}

// Machine tracks one market through its lifecycle.
//
// Every transition requires its condition to hold *continuously* for a dwell
// time, and the thresholds are deliberately separated — list above 75, degrade
// below 60, delist below 40. A single threshold would flap a market on and off
// on noise, and each flap is a real cost to anyone holding a position through
// it. This is v3 §4 made concrete.
type Machine struct {
	cfg   Config
	state State

	// When the current candidate condition first held. Zero when not pending.
	listPending     time.Time
	degradePending  time.Time
	spotOnlyPending time.Time
	recoverPending  time.Time
	delistPending   time.Time

	// Last applied capacity, retained so a re-derivation that changes nothing
	// does not emit a spurious update.
	capacity Capacity
	live     bool

	// Trailing history, so the engine can score what has persisted rather than
	// what is currently on display.
	window *Window
	// Open interest at the last observation, so Close can refuse to settle a
	// market out of existence while positions are still in it.
	lastOI int64
	// Depth at the moment openings stopped, so the size of the unwind can be
	// reported rather than discovered.
	unwindOI, unwindDepth int64
}

func NewMachine(cfg Config) *Machine {
	span := cfg.DepthWindow
	if span <= 0 {
		span = DefaultConfig().DepthWindow
	}
	return &Machine{cfg: cfg, state: Preparing, window: NewWindow(span)}
}

func (m *Machine) State() State       { return m.state }
func (m *Machine) Capacity() Capacity { return m.capacity }

// Transition is an emitted lifecycle change.
type Transition struct {
	At       time.Time
	From, To State
	Score    int64
	Capacity Capacity
	Reason   string
}

// apply installs a derived capacity.
//
// A failed derivation must never be applied as-is: the zero Capacity has an
// initial margin of zero, which converts to *infinite* leverage. Instead the
// previous requirements are retained and openings are switched off, so
// positions stay measured consistently while no new exposure can be added.
// apply installs a derived capacity, capped by what the state permits.
//
// The tier ladder says what the score has earned; the state says what the
// market's condition allows. Taking the lower of the two is what makes a
// downgrade mean something: a market in spot-only does not get 3x back because
// its score bounced for one observation, it gets 3x back by transitioning.
func (m *Machine) apply(c Capacity, ok bool) {
	if !ok {
		// No derivable envelope at all. Keep the previous margins -- a zero
		// initial margin reads as infinite leverage -- and stop openings.
		m.capacity.Openable = false
		return
	}
	if ceil := m.state.Ceiling(); ceil > 0 && c.MaxLeverage > ceil {
		c.MaxLeverage = ceil
		c.InitialBps = BPS / ceil
		// Re-check the invariant the tier loop enforces: initial margin must
		// clear maintenance, or positions open already liquidatable.
		if c.InitialBps <= c.MaintenanceBps {
			m.capacity.Openable = false
			return
		}
	}
	m.capacity = c
}

// Step feeds one observation and returns a transition if the market moved.
//
// Leverage changes inside Live/Degraded are *not* transitions: they are
// continuous adjustment, and they only ever bind new exposure. Nothing here
// force-closes a position — a falling score tightens what can be opened and
// eventually stops openings altogether, but exits stay available throughout.
func (m *Machine) Step(o Observation, now time.Time) *Transition {
	if o.Time.IsZero() {
		o.Time = now
	}
	m.window.Add(o)
	m.lastOI = o.OpenInterest

	// Scored on what the market has sustained, not on what it is showing.
	// Depth and bonded capital are the two signals a listing check can be
	// walked through with rented liquidity, and replacing them with their
	// window floor is what makes renting them expensive: the liquidity has to
	// stay for the dwell, not for the instant of the check.
	eff, _ := m.window.Effective()
	s := Assess(eff, m.cfg)
	cap_, ok := Derive(eff, s, m.cfg)

	prev := m.state

	// Delisting outranks everything: a market this bad should not be allowed
	// to linger in Degraded because the degrade timer has not elapsed.
	//
	// A failed gate counts here regardless of score. A market whose oracle has
	// gone can still be scoring well on depth and maturity and yet be
	// unpriceable, which is not a reason to keep selling exposure in it.
	if s.Total < m.cfg.DelistBelow || !s.Eligible() {
		if m.delistPending.IsZero() {
			m.delistPending = now
		}
		dwell := m.delistDwell(s)
		if now.Sub(m.delistPending) >= dwell &&
			m.state.Openable() {
			m.state = ReduceOnly
			// Keep the last valid margin requirements; only stop openings.
			m.capacity.Openable = false
			m.unwindOI, m.unwindDepth = o.OpenInterest, o.DepthWithin2Pct
			reason := "sustained below delist threshold"
			if !s.Eligible() {
				// Naming the actual cause matters: reporting a gate failure as
				// a low score sends whoever debugs it to the wrong place.
				reason = "sustained gate failure: " + s.GateFailures[0]
			}
			if dwell < m.cfg.DelistSustained {
				reason += fmt.Sprintf(" (accelerated: dwell cut to %s)", dwell.Round(time.Second))
			}
			return m.emit(prev, now, s, reason)
		}
	} else {
		m.delistPending = time.Time{}
	}

	switch m.state {
	case Preparing:
		if ok && s.Total >= m.cfg.ListAbove {
			if m.listPending.IsZero() {
				m.listPending = now
			}
			if now.Sub(m.listPending) >= m.cfg.ListSustained {
				m.state = Live
				m.apply(cap_, ok)
				m.listPending = time.Time{}
				return m.emit(prev, now, s, "sustained above list threshold")
			}
		} else {
			m.listPending = time.Time{}
		}

	case Live:
		if s.Total < m.cfg.DegradeBelow {
			if m.degradePending.IsZero() {
				m.degradePending = now
			}
			if now.Sub(m.degradePending) >= m.cfg.DegradeSustained {
				m.state = Degraded
				m.apply(cap_, ok)
				m.degradePending = time.Time{}
				return m.emit(prev, now, s, "sustained below degrade threshold")
			}
		} else {
			m.degradePending = time.Time{}
		}
		// A market can fall through Degraded without stopping in it: the
		// degrade dwell is there to stop flapping around one threshold, not to
		// hold a collapsing market at 2x while a second timer runs.
		if m.spotOnly(s, eff) {
			if m.spotOnlyPending.IsZero() {
				m.spotOnlyPending = now
			}
			if now.Sub(m.spotOnlyPending) >= m.cfg.SpotOnlySustained {
				m.state = SpotOnly
				m.apply(cap_, ok)
				m.spotOnlyPending, m.degradePending = time.Time{}, time.Time{}
				return m.emit(prev, now, s, "fell past degraded: "+m.spotReason(s, eff))
			}
		} else {
			m.spotOnlyPending = time.Time{}
		}
		// Continuous adjustment within the state.
		m.apply(cap_, ok)

	case Degraded:
		// Down: leverage comes off entirely before the market is closed.
		if m.spotOnly(s, eff) {
			if m.spotOnlyPending.IsZero() {
				m.spotOnlyPending = now
			}
			if now.Sub(m.spotOnlyPending) >= m.cfg.SpotOnlySustained {
				m.state = SpotOnly
				m.apply(cap_, ok)
				m.spotOnlyPending = time.Time{}
				return m.emit(prev, now, s, m.spotReason(s, eff))
			}
		} else {
			m.spotOnlyPending = time.Time{}
		}
		if s.Total >= m.cfg.ListAbove {
			if m.listPending.IsZero() {
				m.listPending = now
			}
			if now.Sub(m.listPending) >= m.cfg.ListSustained {
				m.state = Live
				m.apply(cap_, ok)
				m.listPending = time.Time{}
				return m.emit(prev, now, s, "recovered above list threshold")
			}
		} else {
			m.listPending = time.Time{}
		}
		m.apply(cap_, ok)

	case SpotOnly:
		// Recovery is allowed here, unlike from reduce-only. Spot-only is a
		// downgrade, not a death: the market still has depth, still prices, and
		// still lets people in and out. A market that genuinely recovers should
		// be able to earn leverage back rather than needing to be closed and
		// relisted.
		//
		// It has to clear the *degrade* threshold rather than the spot-only one
		// it fell through, so a score oscillating around the boundary cannot
		// walk leverage back on every upswing.
		if s.Total >= m.cfg.DegradeBelow && !m.spotOnly(s, eff) {
			if m.recoverPending.IsZero() {
				m.recoverPending = now
			}
			if now.Sub(m.recoverPending) >= m.cfg.ListSustained {
				m.state = Degraded
				m.apply(cap_, ok)
				m.recoverPending = time.Time{}
				return m.emit(prev, now, s, "recovered above degrade threshold: leverage restored to 2x")
			}
		} else {
			m.recoverPending = time.Time{}
		}
		m.apply(cap_, ok)

	case ReduceOnly:
		// A market only leaves reduce-only by being closed and re-listed. The
		// alternative — springing back to life on a score blip — is exactly how
		// a manipulator would re-open a market they just drained.

	case Closed:
	}
	m.live = m.state.Openable()
	return nil
}

// emit reports a transition, and only a transition: a state that has not
// changed produces nothing, or a market parked in reduce-only would emit an
// identical update on every observation forever.
func (m *Machine) emit(from State, at time.Time, s Score, reason string) *Transition {
	m.live = m.state.Openable()
	if from == m.state {
		return nil
	}
	return &Transition{
		At: at, From: from, To: m.state,
		Score: s.Total, Capacity: m.capacity, Reason: reason,
	}
}

// spotOnly reports whether this market should be trading without leverage.
//
// Two independent reasons, and a market needs only one of them. A low score
// means the evidence for leverage has gone; a book below the floor means there
// is nothing to liquidate into whatever the evidence says. They are separate
// because a market can be well-established, widely held and perfectly priced
// while nobody is quoting it, and that market should trade at 1x.
//
// Checked here as well as in Derive so the *state* agrees with the leverage.
// Derive alone would leave a lane reporting itself live while offering 1x, and
// a state whose name implies a leverage it does not grant is worse than having
// no state at all.
func (m *Machine) spotOnly(s Score, o Observation) bool {
	if s.Total < m.cfg.SpotOnlyBelow {
		return true
	}
	// A price we cannot lend against. Checked here as well as in Derive, or the
	// lane reports itself live while offering 1x -- which was exactly the
	// inconsistency this method was added to prevent, reintroduced one field
	// later.
	if !s.Leverable() {
		return true
	}
	return m.cfg.MinBookForLeverage > 0 &&
		o.BookDepthWithin2Pct < m.cfg.MinBookForLeverage
}

// delistDwell is how long a delist condition must hold before openings stop.
//
// A constant dwell is wrong in both directions at once. Two hours is far too
// long for a market losing its liquidity in minutes -- the whole delisting
// arrives after the money has gone -- and too short would flap every market on
// ordinary noise.
//
// So the dwell scales with how bad the reading is. A market drifting just under
// the threshold gets the full patient treatment. One that has fallen far below
// it, or whose depth has collapsed within the window, gets a fraction of it.
// Severity is taken as the worse of the two, because they fail independently: a
// score can crater while depth holds (an oracle outage) and depth can crater
// while the score lags (a pull the other signals have not caught up with).
func (m *Machine) delistDwell(s Score) time.Duration {
	full := m.cfg.DelistSustained
	floor := full / 8
	if m.cfg.DelistMinDwell > 0 {
		floor = m.cfg.DelistMinDwell
	}
	if floor > full {
		floor = full
	}

	// How far below the threshold, as a fraction of the threshold itself.
	var byScore float64
	if m.cfg.DelistBelow > 0 && s.Total < m.cfg.DelistBelow {
		byScore = float64(m.cfg.DelistBelow-s.Total) / float64(m.cfg.DelistBelow)
	}
	// How far depth has fallen from its peak in the window.
	var byDepth float64
	if drop := -m.window.DepthChangeBps(); drop > 0 {
		byDepth = float64(drop) / float64(m.cfg.DelistDepthDropBps)
	}

	sev := byScore
	if byDepth > sev {
		sev = byDepth
	}
	if sev <= 0 {
		return full
	}
	if sev >= 1 {
		return floor
	}
	return floor + time.Duration(float64(full-floor)*(1-sev))
}

// UnwindPressure reports how violent leaving this market will be: open interest
// as a multiple of the depth available to absorb it, at the moment openings
// stopped. Zero when the market is not unwinding.
//
// Reduce-only is the correct response to a failing market and it is also the
// moment every position starts heading for the same exit. On a market whose
// depth has already gone, that exit is the liquidation cascade -- so the size of
// it is worth reporting rather than discovering.
func (m *Machine) UnwindPressure() float64 {
	if m.state != ReduceOnly || m.unwindDepth <= 0 {
		return 0
	}
	return float64(m.unwindOI) / float64(m.unwindDepth)
}

// Close settles a reduce-only market out of existence.
//
// Refuses while open interest remains. The old version took the market's word
// for it and reported "positions settled" without checking, which would strand
// every open position in a lane nobody is executing any more -- and the whole
// point of reduce-only is to give those positions somewhere to go first.
func (m *Machine) Close(now time.Time) *Transition {
	if m.state != ReduceOnly || m.lastOI != 0 {
		return nil
	}
	prev := m.state
	m.state = Closed
	return &Transition{At: now, From: prev, To: Closed, Reason: "positions settled"}
}

// CloseBlocked reports why Close refused, for an operator asking.
func (m *Machine) CloseBlocked() (bool, string) {
	switch {
	case m.state != ReduceOnly:
		return true, "market is " + m.state.String() + ", not reduce-only"
	case m.lastOI != 0:
		return true, fmt.Sprintf("%d open interest remains", m.lastOI)
	}
	return false, ""
}
