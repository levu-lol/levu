package health

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

func obsAt(min int, depth int64) Observation {
	o := Observation{
		Time:                t0.Add(time.Duration(min) * time.Minute),
		DepthWithin2Pct:     depth,
		BookDepthWithin2Pct: depth / 3,
		UnderwrittenCapital: 500_000,
		Age:                 60 * 24 * time.Hour,
		TopHolderShareBps:   800,
		TopPoolShareBps:     3_000,
		OracleConfidence:    9_000,
		Volume24h:           10_000_000,
		SpotTVL:             5_000_000,
		MarketCap:           80_000_000,
		RealisedVolBps:      8_000,
	}
	return o
}

// The headline property: depth that showed up for the listing check and left
// again must not be scored as if it were still there.
func TestRentedDepthDoesNotCount(t *testing.T) {
	w := NewWindow(time.Hour)
	// An hour of thin depth, then one deep reading.
	for i := 0; i < 12; i++ {
		w.Add(obsAt(i*5, 50_000))
	}
	w.Add(obsAt(60, 5_000_000))

	latest, _ := w.Latest()
	if latest.DepthWithin2Pct != 5_000_000 {
		t.Fatal("test setup wrong")
	}
	eff, _ := w.Effective()
	if eff.DepthWithin2Pct > 100_000 {
		t.Fatalf("a single deep reading raised the effective depth to %d; "+
			"renting liquidity for one observation would pass the check", eff.DepthWithin2Pct)
	}
}

// And the converse: depth that has genuinely been there must be usable, or the
// engine would never list anything.
func TestSustainedDepthCounts(t *testing.T) {
	w := NewWindow(time.Hour)
	for i := 0; i < 13; i++ {
		w.Add(obsAt(i*5, 2_000_000))
	}
	eff, _ := w.Effective()
	if eff.DepthWithin2Pct != 2_000_000 {
		t.Fatalf("sustained depth was discounted to %d", eff.DepthWithin2Pct)
	}
}

// One bad sample from an upstream must not poison the window. This is why the
// floor is a percentile and not the minimum.
func TestASingleBadSampleDoesNotPoisonTheWindow(t *testing.T) {
	w := NewWindow(time.Hour)
	for i := 0; i < 12; i++ {
		w.Add(obsAt(i*5, 2_000_000))
	}
	w.Add(obsAt(60, 1)) // an RPC hiccup

	eff, _ := w.Effective()
	if eff.DepthWithin2Pct < 1_000_000 {
		t.Fatalf("one bad reading dropped effective depth to %d; "+
			"operators would learn to override an engine this jumpy", eff.DepthWithin2Pct)
	}
}

func TestTheWindowDropsWhatIsTooOld(t *testing.T) {
	w := NewWindow(30 * time.Minute)
	for i := 0; i <= 12; i++ {
		w.Add(obsAt(i*5, 1_000_000))
	}
	if got := w.Covered(); got > 30*time.Minute {
		t.Fatalf("window holds %s of history, span is 30m", got)
	}
	if w.Len() == 13 {
		t.Fatal("nothing was evicted")
	}
}

// Change is measured from the peak, not the first reading: a collapse already
// under way when the window opened would otherwise look flat.
func TestDepthChangeIsMeasuredFromThePeak(t *testing.T) {
	w := NewWindow(time.Hour)
	w.Add(obsAt(0, 1_000_000))
	w.Add(obsAt(10, 4_000_000))
	w.Add(obsAt(20, 2_000_000))

	if got := w.DepthChangeBps(); got != -5_000 {
		t.Fatalf("change from peak = %d bps, want -5000", got)
	}
}

func TestAnEmptyWindowIsHarmless(t *testing.T) {
	w := NewWindow(time.Hour)
	if _, ok := w.Effective(); ok {
		t.Error("an empty window reported an observation")
	}
	if w.DepthFloor() != 0 || w.DepthChangeBps() != 0 || w.Covered() != 0 {
		t.Error("an empty window produced non-zero readings")
	}
}

// Severity has to drive the dwell, or a market losing its liquidity in minutes
// waits out the same patient timer as one drifting gently.
func TestDelistDwellScalesWithSeverity(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)

	// A score just under the threshold, with depth intact: near-full patience.
	// Not exactly full -- the scaling is continuous on purpose, so there is no
	// cliff for a market sitting a point either side of the line.
	mild := m.delistDwell(Score{Total: cfg.DelistBelow - 1})
	if slack := cfg.DelistSustained - mild; slack < 0 || slack > time.Minute {
		t.Errorf("a marginal reading got %s, want within a minute of the full %s",
			mild, cfg.DelistSustained)
	}

	// A score at the floor: minimum dwell.
	severe := m.delistDwell(Score{Total: 0})
	if severe != cfg.DelistMinDwell {
		t.Errorf("a catastrophic score got %s, want the %s floor", severe, cfg.DelistMinDwell)
	}
	if severe >= mild {
		t.Fatal("severity did not shorten the dwell at all")
	}

	// Halfway down should land between the two, not at either end.
	mid := m.delistDwell(Score{Total: cfg.DelistBelow / 2})
	if mid <= severe || mid >= mild {
		t.Errorf("a mid reading got %s, expected between %s and %s", mid, severe, mild)
	}
}

// Depth collapse must accelerate the dwell on its own, because it fails
// independently of the score: liquidity can go before the other signals notice.
func TestACollapseAcceleratesEvenOnAHealthyScore(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	for i := 0; i < 10; i++ {
		m.window.Add(obsAt(i*5, 4_000_000))
	}
	m.window.Add(obsAt(55, 100_000)) // liquidity pulled

	healthy := Score{Total: cfg.DelistBelow - 1}
	if got := m.delistDwell(healthy); got != cfg.DelistMinDwell {
		t.Fatalf("a 97%% depth collapse got dwell %s, want the %s floor", got, cfg.DelistMinDwell)
	}
}

// Close claimed "positions settled" without checking. Settling a market out of
// existence while positions are open strands every one of them.
func TestCloseRefusesWhilePositionsRemain(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	m.state = ReduceOnly
	m.lastOI = 250_000

	if tr := m.Close(t0); tr != nil {
		t.Fatal("closed a market with open interest still in it")
	}
	blocked, why := m.CloseBlocked()
	if !blocked || why == "" {
		t.Fatalf("Close refused without saying why: %v %q", blocked, why)
	}

	m.lastOI = 0
	tr := m.Close(t0)
	if tr == nil {
		t.Fatal("refused to close an empty reduce-only market")
	}
	if tr.To != Closed {
		t.Fatalf("closed into state %s", tr.To)
	}
	if blocked, _ := m.CloseBlocked(); !blocked {
		t.Error("an already-closed market should not report itself closeable")
	}
}

// Reduce-only is the moment every position heads for the same exit. How
// violent that will be is worth reporting rather than discovering.
func TestUnwindPressureIsReported(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	if m.UnwindPressure() != 0 {
		t.Error("a market that is not unwinding reported pressure")
	}
	m.state = ReduceOnly
	m.unwindOI, m.unwindDepth = 900_000, 300_000
	if got := m.UnwindPressure(); got != 3 {
		t.Fatalf("unwind pressure = %v, want 3 (OI is 3x the depth to absorb it)", got)
	}
}

// A config whose window is shorter than its listing dwell would let depth be
// rented for the check and pulled before the window noticed.
func TestValidateRefusesAWindowShorterThanTheListingDwell(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DepthWindow = time.Minute
	cfg.ListSustained = 30 * time.Minute
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a window shorter than the listing dwell")
	}

	cfg = DefaultConfig()
	cfg.DepthWindow = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a zero window, which makes rented liquidity free again")
	}

	cfg = DefaultConfig()
	cfg.DelistMinDwell = cfg.DelistSustained + time.Hour
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted a minimum dwell longer than the maximum")
	}

	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the shipped default is invalid: %v", err)
	}
}
