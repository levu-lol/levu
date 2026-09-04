package health

import "testing"

// Missing data must never score better than measured data.
//
// The natural reading of a zero is "none of this bad thing", which hands full
// marks to whoever supplied nothing. Neither concentration nor volatility is
// plausibly zero in a live market -- there is always a largest holder, always a
// deepest pool, and a price that never moves is a price nobody trades -- so a
// zero is missing data and is scored as the worst case.
func TestUnsuppliedFieldsAreNeverOptimistic(t *testing.T) {
	cfg := DefaultConfig()
	base := baseObs()
	full := Assess(base, cfg)

	for _, tc := range []struct {
		name string
		mut  func(*Observation)
	}{
		{"Age", func(o *Observation) { o.Age = 0 }},
		{"TopHolderShareBps", func(o *Observation) { o.TopHolderShareBps = 0 }},
		{"TopPoolShareBps", func(o *Observation) { o.TopPoolShareBps = 0 }},
		{"RealisedVolBps", func(o *Observation) { o.RealisedVolBps = 0 }},
		{"UnderwrittenCapital", func(o *Observation) { o.UnderwrittenCapital = 0 }},
		{"everything unsupplied", func(o *Observation) {
			o.Age, o.TopHolderShareBps, o.TopPoolShareBps = 0, 0, 0
			o.RealisedVolBps, o.UnderwrittenCapital = 0, 0
		}},
	} {
		o := base
		tc.mut(&o)
		s := Assess(o, cfg)
		if s.Total > full.Total {
			t.Errorf("leaving %s unsupplied scored %d, better than the measured %d: "+
				"an operator with no data would get a better market than one with data",
				tc.name, s.Total, full.Total)
		}
	}
}

// And the worst case really is the worst case: a market with everything
// unsupplied must not clear the listing bar.
func TestAMarketWithNoSuppliedFactsDoesNotList(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		DepthWithin2Pct:     10_000_000, // pristine on-chain readings
		BookDepthWithin2Pct: 5_000_000,
		OracleConfidence:    10_000,
		Volume24h:           50_000_000,
		SpotTVL:             20_000_000,
		MarketCap:           500_000_000,
		// Everything a pool cannot answer, left unsupplied.
	}
	s := Assess(o, cfg)
	if s.Eligible() && s.Total >= cfg.ListAbove {
		t.Fatalf("a market with no supplied facts scored %d and would list", s.Total)
	}
}
