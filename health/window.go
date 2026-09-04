package health

import (
	"sort"
	"time"
)

// Window is a market's recent history, kept so the engine can reason about
// change rather than only about level.
//
// Every signal in Observation is a snapshot, and snapshots cannot distinguish
// the two situations that matter most at the edges of a market's life:
//
//   - Depth of $1M and rising, versus depth of $1M and collapsing. Identical
//     score, opposite meaning. For delisting the derivative is the leading
//     indicator and the level is the lagging one, so an engine that reads only
//     levels is always late.
//   - Depth that has been there for a day, versus depth that arrived a minute
//     ago to pass a listing check. A single reading cannot tell them apart, and
//     the second is cheap to manufacture: add liquidity, get listed, pull it.
//
// So the window is not an optimisation. It is what makes the two costly signals
// actually costly.
type Window struct {
	span time.Duration
	obs  []Observation
}

func NewWindow(span time.Duration) *Window { return &Window{span: span} }

// Add records an observation and drops anything older than the span.
func (w *Window) Add(o Observation) {
	w.obs = append(w.obs, o)
	if o.Time.IsZero() {
		return
	}
	cut := o.Time.Add(-w.span)
	i := 0
	for i < len(w.obs) && !w.obs[i].Time.IsZero() && w.obs[i].Time.Before(cut) {
		i++
	}
	if i > 0 {
		w.obs = append(w.obs[:0], w.obs[i:]...)
	}
}

func (w *Window) Len() int { return len(w.obs) }

// Covered is how much history the window actually holds, which is less than
// its span for a market that has only just appeared.
func (w *Window) Covered() time.Duration {
	if len(w.obs) < 2 {
		return 0
	}
	first, last := w.obs[0].Time, w.obs[len(w.obs)-1].Time
	if first.IsZero() || last.IsZero() {
		return 0
	}
	return last.Sub(first)
}

// DepthFloor is the depth this market has actually sustained: a low percentile
// of the window rather than the latest reading.
//
// A percentile and not the minimum. The minimum is the more obvious choice and
// it is wrong in practice: one bad sample -- an RPC hiccup, a single block
// mid-swap -- would poison the score for the whole span, and an engine that
// punishes a market for its worst millisecond is an engine operators learn to
// override. The tenth percentile still requires liquidity to persist while
// tolerating the occasional lie from an upstream.
//
// With a short history this converges on the minimum, which is the correct
// behaviour for a market nobody has watched for long yet.
func (w *Window) DepthFloor() int64 {
	return percentile(w.values(func(o Observation) int64 { return o.DepthWithin2Pct }), 10)
}

// UnderwritingFloor applies the same reasoning to bonded capital, which can
// also be posted for a listing check and withdrawn afterwards.
func (w *Window) UnderwritingFloor() int64 {
	return percentile(w.values(func(o Observation) int64 { return o.UnderwrittenCapital }), 10)
}

// DepthChangeBps is how far depth has moved from the start of the window to
// now, in basis points. Negative means falling.
//
// Measured from the window's highest reading rather than its first, because a
// collapse that began before the window opened would otherwise look flat.
func (w *Window) DepthChangeBps() int64 {
	if len(w.obs) < 2 {
		return 0
	}
	var peak int64
	for _, o := range w.obs {
		if o.DepthWithin2Pct > peak {
			peak = o.DepthWithin2Pct
		}
	}
	if peak <= 0 {
		return 0
	}
	last := w.obs[len(w.obs)-1].DepthWithin2Pct
	return ((last - peak) * 10_000) / peak
}

// Latest is the newest observation.
func (w *Window) Latest() (Observation, bool) {
	if len(w.obs) == 0 {
		return Observation{}, false
	}
	return w.obs[len(w.obs)-1], true
}

func (w *Window) values(pick func(Observation) int64) []int64 {
	out := make([]int64, 0, len(w.obs))
	for _, o := range w.obs {
		out = append(out, pick(o))
	}
	return out
}

// percentile returns the p-th percentile by nearest rank.
func percentile(v []int64, p int) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p * len(s)) / 100
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// Effective is the observation the engine should actually score: the latest
// reading, with the two counterfeitable levels replaced by what has persisted.
//
// Everything else passes through unchanged. Volatility, oracle confidence and
// concentration are already measured over a period by whoever produced them,
// and open interest is a fact about now, not a claim about the past.
func (w *Window) Effective() (Observation, bool) {
	o, ok := w.Latest()
	if !ok {
		return o, false
	}
	// The floor replaces the reading outright, in both directions.
	//
	// Taking min(latest, floor) is the tempting version and it is wrong: it
	// leaves a spuriously low sample passing straight through, which defeats
	// the percentile in exactly the case the percentile exists for. What the
	// score wants is one thing -- the depth this market has actually sustained
	// -- not a blend of that and whatever the last poll happened to see.
	//
	// The cost is that a genuine collapse takes time to pull the score down,
	// and that cost is paid elsewhere on purpose: the delist severity reads
	// DepthChangeBps, which compares the *latest* reading against the window
	// peak, and the gates read the latest observation untouched. So a market
	// that loses its liquidity is caught by the accelerated dwell and by the
	// TVL gate within one observation, while the slow-moving score stays
	// immune to a single lie about depth.
	o.DepthWithin2Pct = w.DepthFloor()
	o.UnderwrittenCapital = w.UnderwritingFloor()
	return o, true
}
