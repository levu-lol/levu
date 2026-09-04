package health

import (
	"testing"
	"time"
)

// Hysteresis exists so a market does not flap on noise. Each flap is a real
// cost to anyone holding a position through it, so a condition must hold
// *continuously* for its dwell time — a score that keeps dipping never lists.
func TestAnOscillatingScoreNeverLists(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)

	high := baseObs()
	// Below the list threshold but above the delist one, so the test exercises
	// the listing dwell timer rather than the delisting path.
	low := baseObs()
	low.DepthWithin2Pct = 100_000
	low.BookDepthWithin2Pct = 40_000
	low.UnderwrittenCapital = 200_000
	low.OracleConfidence = 6_000
	low.TopPoolShareBps = 6_500
	low.RealisedVolBps = 26_000

	if Assess(high, cfg).Total < cfg.ListAbove {
		t.Fatal("fixture: high observation should be above the list threshold")
	}
	if lo := Assess(low, cfg).Total; lo >= cfg.ListAbove || lo < cfg.DelistBelow {
		t.Fatalf("fixture: low score %d should sit between delist %d and list %d",
			lo, cfg.DelistBelow, cfg.ListAbove)
	}

	now := epoch
	for i := 0; i < 40; i++ {
		o := high
		if i%2 == 1 {
			o = low
		}
		o.Time = now
		if tr := m.Step(o, now); tr != nil {
			t.Fatalf("listed at step %d despite oscillating: %+v", i, tr)
		}
		now = now.Add(10 * time.Minute)
	}
	if m.State() != Preparing {
		t.Errorf("state = %s, want preparing", m.State())
	}
}

func TestASustainedGoodScoreListsAfterTheDwellTime(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	o := baseObs()
	now := epoch

	// Just short of the dwell time: not yet.
	for now.Sub(epoch) < cfg.ListSustained {
		o.Time = now
		if tr := m.Step(o, now); tr != nil {
			t.Fatalf("listed after %v, before the %v dwell time", now.Sub(epoch), cfg.ListSustained)
		}
		now = now.Add(5 * time.Minute)
	}
	o.Time = now
	tr := m.Step(o, now)
	if tr == nil || tr.To != Live {
		t.Fatalf("expected listing once the dwell time elapsed, got %+v", tr)
	}
}

// Leverage changes within Live are continuous adjustment, not transitions: the
// control plane should not see a lifecycle event every time the cap moves.
func TestLeverageAdjustsWithoutEmittingTransitions(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	o := baseObs()
	o.DepthWithin2Pct = 5_000_000
	o.BookDepthWithin2Pct = 2_000_000 // makers have quoted it properly
	o.UnderwrittenCapital = 2_000_000
	o.OracleConfidence = 10_000
	o.TopHolderShareBps = 100
	o.TopPoolShareBps = 500
	o.RealisedVolBps = 1_000

	now := epoch
	for i := 0; i < 10; i++ {
		o.Time = now
		m.Step(o, now)
		now = now.Add(10 * time.Minute)
	}
	// Live, and carrying real leverage. Not a specific number: leverage is a
	// consequence of how cheaply this book can be closed, so pinning it here
	// would assert the tuning rather than the behaviour under test, which is
	// that the envelope tracks the book without emitting transitions.
	if m.State() != Live {
		t.Fatalf("expected live, got %s", m.State())
	}
	if lev := m.Capacity().MaxLeverage; lev < 5 {
		t.Fatalf("a deep, well-scored market got only %dx", lev)
	}

	before := m.Capacity()

	// Thin the book out: the cap falls with no transition emitted.
	//
	// The cap is what this asserts, not the leverage tier. Leverage used to
	// fall here because thinning the *venue's* depth dropped the health score
	// across a tier boundary, and that coupling was deliberately removed: the
	// venue's depth is not what backs a position. What must fall is the
	// exposure the lane will carry, and it does, because that comes from the
	// book a liquidation would actually eat.
	// Underwriting is held constant so this isolates the book. Cutting both at
	// once made maintenance *fall*, correctly: the cap shrank faster than the
	// book did, so closing the largest position got relatively easier.
	o.DepthWithin2Pct = 900_000
	o.BookDepthWithin2Pct = 300_000
	for i := 0; i < 3; i++ {
		o.Time = now
		if tr := m.Step(o, now); tr != nil {
			t.Fatalf("continuous adjustment emitted a transition: %+v", tr)
		}
		now = now.Add(10 * time.Minute)
	}
	after := m.Capacity()
	if after.OICap >= before.OICap {
		t.Errorf("cap did not fall as the book thinned: %d -> %d", before.OICap, after.OICap)
	}
	if after.MaintenanceBps < before.MaintenanceBps {
		t.Errorf("maintenance fell on a thinner book: %d -> %d",
			before.MaintenanceBps, after.MaintenanceBps)
	}

	// And when the book gets too thin to lever against, the leverage goes and
	// the market stays open at 1x.
	//
	// Not closed: a 20k book is a real market that nobody should be borrowing
	// against, which is a different thing from a market nobody should be in.
	// Traders keep a way in and a way out; they just do it at full collateral.
	o.BookDepthWithin2Pct = 20_000
	for i := 0; i < 40; i++ {
		o.Time = now
		m.Step(o, now)
		now = now.Add(5 * time.Minute)
	}
	if m.State() != SpotOnly {
		t.Errorf("a lane with a 20k book is %s, want spot-only", m.State())
	}
	if !m.Capacity().Openable {
		t.Error("spot-only stopped accepting positions; that is reduce-only, not spot")
	}
	if lev := m.Capacity().MaxLeverage; lev != 1 {
		t.Errorf("a 20k book still offers %dx", lev)
	}
	_ = before
}

// A market only leaves reduce-only by being closed and re-listed. Springing
// back on a score blip is how a manipulator re-opens a market they just drained.
func TestReduceOnlyDoesNotSpontaneouslyRecover(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)

	good := baseObs()
	now := epoch
	for i := 0; i < 8; i++ {
		good.Time = now
		m.Step(good, now)
		now = now.Add(10 * time.Minute)
	}
	if m.State() != Live {
		t.Fatalf("expected live, got %s", m.State())
	}

	bad := good
	// A hard gate, not a leverage blocker. Low confidence now withdraws
	// leverage rather than the market, so it would land in spot-only and this
	// test is about a market that must not come back at all.
	bad.Volume24h = 0
	for i := 0; i < 30; i++ {
		bad.Time = now
		m.Step(bad, now)
		now = now.Add(15 * time.Minute)
	}
	if m.State() != ReduceOnly {
		t.Fatalf("expected reduce-only, got %s", m.State())
	}

	// Full recovery on every metric must not bring it back.
	for i := 0; i < 40; i++ {
		good.Time = now
		if tr := m.Step(good, now); tr != nil {
			t.Fatalf("reduce-only market recovered on its own: %+v", tr)
		}
		now = now.Add(15 * time.Minute)
	}
	if m.State() != ReduceOnly {
		t.Errorf("state = %s, want reduce-only", m.State())
	}
}

func TestClosingOnlyWorksFromReduceOnly(t *testing.T) {
	m := NewMachine(DefaultConfig())
	if tr := m.Close(epoch); tr != nil {
		t.Error("a preparing market should not be closable")
	}
}
