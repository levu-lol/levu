package oracle

import (
	"testing"
	"time"

	"github.com/levu-lol/levu/wire"
)

var now = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func src(name string, price, liq int64) Source {
	return Source{
		Name:      name,
		Price:     wire.FixedWhole(price),
		Liquidity: wire.FixedWhole(liq),
		Observed:  now,
	}
}

func TestSingleSourceIsUsedButScoresLowCoverage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSources = 1
	r := Aggregate([]Source{src("uniswap", 100, 1_000_000)}, cfg, now)
	if r.Price.Cmp(wire.FixedWhole(100)) != 0 {
		t.Fatalf("price = %s, want 100", r.Price)
	}
	if !r.Healthy {
		t.Error("one source should be usable when MinSources is 1")
	}
	// Coverage is 1/4, depth and agreement are 1, so confidence is 2500.
	if r.Confidence != 2500 {
		t.Errorf("confidence = %d, want 2500 (a lone venue is a single point of failure)", r.Confidence)
	}
}

// The central property: a thin venue cannot outvote a deep one.
func TestLiquidityWeightingBeatsSourceCount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDeviation = wire.FixedRawInt64(500_000_000_000_000_000) // 50%, so nothing is dropped
	r := Aggregate([]Source{
		src("thin_a", 150, 1_000),
		src("thin_b", 150, 1_000),
		src("deep", 100, 5_000_000),
	}, cfg, now)
	if r.Price.Cmp(wire.FixedWhole(100)) != 0 {
		t.Fatalf("price = %s, want 100: two thin venues must not outvote a deep one", r.Price)
	}
}

// The manipulation case: an attacker stands up a venue quoting 10x. It carries
// almost no depth, and is beyond the deviation guard, so it is discarded.
func TestAnOutlierVenueIsDiscarded(t *testing.T) {
	cfg := DefaultConfig()
	r := Aggregate([]Source{
		src("uniswap", 100, 2_000_000),
		src("binance", 101, 5_000_000),
		src("attacker", 1000, 5_000),
	}, cfg, now)

	for _, u := range r.Used {
		if u == "attacker" {
			t.Fatal("the manipulated venue was used")
		}
	}
	if r.Price.Cmp(wire.FixedWhole(101)) > 0 || r.Price.Cmp(wire.FixedWhole(100)) < 0 {
		t.Errorf("price = %s, want the honest 100..101 range", r.Price)
	}
	if len(r.Reject) == 0 {
		t.Error("the rejection should be reported, not silent")
	}
}

// An attacker who *does* carry depth still moves the price — there is no
// defence at the aggregator level against genuinely buying the market. What
// bounds the damage is the mark band and the leverage the health engine grants.
func TestADeepAttackerDoesMoveThePriceAndConfidenceDrops(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDeviation = wire.FixedRawInt64(500_000_000_000_000_000) // 50%
	honest := Aggregate([]Source{
		src("uniswap", 100, 2_000_000),
		src("binance", 100, 2_000_000),
	}, cfg, now)
	attacked := Aggregate([]Source{
		src("uniswap", 100, 2_000_000),
		src("binance", 100, 2_000_000),
		src("attacker", 130, 3_000_000),
	}, cfg, now)

	if attacked.Confidence >= honest.Confidence {
		t.Errorf("confidence must fall when venues disagree: %d -> %d",
			honest.Confidence, attacked.Confidence)
	}
}

func TestStaleSourcesAreDropped(t *testing.T) {
	cfg := DefaultConfig()
	old := src("stale", 500, 5_000_000)
	old.Observed = now.Add(-time.Hour)
	r := Aggregate([]Source{
		src("a", 100, 1_000_000),
		src("b", 100, 1_000_000),
		old,
	}, cfg, now)
	if r.Price.Cmp(wire.FixedWhole(100)) != 0 {
		t.Errorf("price = %s: a stale quote must not set the index", r.Price)
	}
	for _, u := range r.Used {
		if u == "stale" {
			t.Fatal("stale source was used")
		}
	}
}

func TestAllSourcesStaleYieldsNoReading(t *testing.T) {
	cfg := DefaultConfig()
	a, b := src("a", 100, 1_000_000), src("b", 100, 1_000_000)
	a.Observed = now.Add(-time.Hour)
	b.Observed = now.Add(-time.Hour)
	r := Aggregate([]Source{a, b}, cfg, now)
	if r.Healthy || r.Confidence != 0 || !r.Price.IsZero() {
		t.Errorf("expected no reading, got price=%s conf=%d healthy=%v",
			r.Price, r.Confidence, r.Healthy)
	}
}

func TestBelowMinSourcesIsUnhealthy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSources = 3
	r := Aggregate([]Source{
		src("a", 100, 1_000_000),
		src("b", 100, 1_000_000),
	}, cfg, now)
	if r.Healthy {
		t.Error("two sources must not satisfy a floor of three")
	}
	if r.Price.IsZero() {
		t.Error("an unhealthy reading should still report what it computed")
	}
}

func TestNonPositiveInputsAreRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSources = 1
	r := Aggregate([]Source{
		{Name: "zero_price", Price: wire.FixedZero(), Liquidity: wire.FixedWhole(1), Observed: now},
		{Name: "zero_liq", Price: wire.FixedWhole(100), Liquidity: wire.FixedZero(), Observed: now},
		src("good", 100, 1_000_000),
	}, cfg, now)
	if len(r.Used) != 1 || r.Used[0] != "good" {
		t.Errorf("used = %v, want only the valid source", r.Used)
	}
	if len(r.Reject) != 2 {
		t.Errorf("expected both invalid sources reported, got %v", r.Reject)
	}
}

func TestNoSourcesAtAll(t *testing.T) {
	r := Aggregate(nil, DefaultConfig(), now)
	if r.Healthy || r.Confidence != 0 {
		t.Error("no sources means no reading")
	}
}

func TestConfidenceRisesWithCoverageAndDepth(t *testing.T) {
	cfg := DefaultConfig()
	small := Aggregate([]Source{
		src("a", 100, 100_000),
		src("b", 100, 100_000),
	}, cfg, now)
	full := Aggregate([]Source{
		src("a", 100, 1_000_000),
		src("b", 100, 1_000_000),
		src("c", 100, 1_000_000),
		src("d", 100, 1_000_000),
	}, cfg, now)

	if full.Confidence <= small.Confidence {
		t.Errorf("more sources and more depth must raise confidence: %d vs %d",
			small.Confidence, full.Confidence)
	}
	if full.Confidence != 10_000 {
		t.Errorf("four unanimous deep sources should be full confidence, got %d", full.Confidence)
	}
}

func TestConfidenceFallsWithDisagreement(t *testing.T) {
	cfg := DefaultConfig()
	unanimous := Aggregate([]Source{
		src("a", 100, 1_000_000), src("b", 100, 1_000_000),
		src("c", 100, 1_000_000), src("d", 100, 1_000_000),
	}, cfg, now)
	split := Aggregate([]Source{
		src("a", 98, 1_000_000), src("b", 100, 1_000_000),
		src("c", 102, 1_000_000), src("d", 104, 1_000_000),
	}, cfg, now)

	if split.Confidence >= unanimous.Confidence {
		t.Errorf("dispersion must lower confidence: %d vs %d",
			unanimous.Confidence, split.Confidence)
	}
	if split.Agreement.Cmp(wire.FixedOne()) >= 0 {
		t.Error("agreement should be below 1 when venues disagree")
	}
}

// Determinism: the result must not depend on the order observations arrive in,
// including when two venues quote exactly the same price.
func TestResultIsIndependentOfInputOrder(t *testing.T) {
	cfg := DefaultConfig()
	a := []Source{
		src("a", 100, 1_000_000),
		src("b", 100, 2_000_000),
		src("c", 101, 1_500_000),
	}
	b := []Source{a[2], a[0], a[1]}
	c := []Source{a[1], a[2], a[0]}

	ra, rb, rc := Aggregate(a, cfg, now), Aggregate(b, cfg, now), Aggregate(c, cfg, now)
	if ra.Price.Cmp(rb.Price) != 0 || ra.Price.Cmp(rc.Price) != 0 {
		t.Errorf("order changed the price: %s / %s / %s", ra.Price, rb.Price, rc.Price)
	}
	if ra.Confidence != rb.Confidence || ra.Confidence != rc.Confidence {
		t.Errorf("order changed the confidence: %d / %d / %d",
			ra.Confidence, rb.Confidence, rc.Confidence)
	}
}

func TestWeightedMedianPicksTheCrossingPoint(t *testing.T) {
	// Total 100. Half is 50. Cumulative: 10 (at 1), 40 (at 2), 100 (at 3).
	// First crossing 50 is at price 3.
	got := weightedMedian([]Source{
		src("x", 1, 10), src("y", 2, 30), src("z", 3, 60),
	})
	if got.Cmp(wire.FixedWhole(3)) != 0 {
		t.Errorf("weighted median = %s, want 3", got)
	}
}

func TestWeightedMedianWithEqualWeightsIsTheLowerMiddle(t *testing.T) {
	got := weightedMedian([]Source{
		src("a", 10, 1), src("b", 20, 1), src("c", 30, 1), src("d", 40, 1),
	})
	if got.Cmp(wire.FixedWhole(20)) != 0 {
		t.Errorf("weighted median = %s, want the lower middle 20", got)
	}
}

// Coverage must not be purchasable. An adversary who stands up venues — either
// disagreeing ones, or empty ones parroting the honest price — must not be able
// to raise the confidence the VM will act on.
func TestConfidenceCannotBeRaisedByAddingSources(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDeviation = wire.FixedRawInt64(500_000_000_000_000_000) // 50%, nothing dropped

	honest := []Source{
		src("uniswap", 100, 2_000_000),
		src("binance", 100, 2_000_000),
	}
	base := Aggregate(honest, cfg, now)

	t.Run("disagreeing venue", func(t *testing.T) {
		withAttacker := Aggregate(append(append([]Source{}, honest...),
			src("attacker", 130, 3_000_000)), cfg, now)
		if withAttacker.Confidence >= base.Confidence {
			t.Errorf("a disagreeing venue raised confidence: %d -> %d",
				base.Confidence, withAttacker.Confidence)
		}
	})

	t.Run("sybil venues at the honest price", func(t *testing.T) {
		sybil := append([]Source{}, honest...)
		for i := 0; i < 20; i++ {
			sybil = append(sybil, src(string(rune('A'+i))+"_sybil", 100, 1_000))
		}
		r := Aggregate(sybil, cfg, now)
		// Twenty empty venues add 20*1000/250000 = 0.08 effective observations.
		if r.Confidence > base.Confidence+200 {
			t.Errorf("empty venues manufactured confidence: %d -> %d",
				base.Confidence, r.Confidence)
		}
	})
}

// The honest counterpart: a genuinely deep, agreeing venue *should* raise
// confidence, or the metric measures nothing.
func TestAGenuineDeepVenueRaisesConfidence(t *testing.T) {
	cfg := DefaultConfig()
	base := Aggregate([]Source{
		src("uniswap", 100, 2_000_000),
		src("binance", 100, 2_000_000),
	}, cfg, now)
	better := Aggregate([]Source{
		src("uniswap", 100, 2_000_000),
		src("binance", 100, 2_000_000),
		src("coinbase", 100, 2_000_000),
	}, cfg, now)
	if better.Confidence <= base.Confidence {
		t.Errorf("a real agreeing venue must raise confidence: %d -> %d",
			base.Confidence, better.Confidence)
	}
}
