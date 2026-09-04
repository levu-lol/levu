package health

import (
	"math"
	"testing"
	"time"
)

func baseObs() Observation {
	return Observation{
		Time:                epoch,
		DepthWithin2Pct:     1_000_000,
		BookDepthWithin2Pct: 350_000,
		UnderwrittenCapital: 500_000,
		Age:                 30 * 24 * time.Hour,
		TopHolderShareBps:   1_000,
		TopPoolShareBps:     3_000,
		OracleConfidence:    9_000,
		Volume24h:           5_000_000,
		SpotTVL:             3_000_000,
		MarketCap:           50_000_000,
		RealisedVolBps:      10_000,
	}
}

// Regression: Age is a time.Duration in nanoseconds, so a 200-day-old token is
// 1.7e16. Multiplying by BPS overflows int64 and wraps, silently scoring a
// mature token as immature. This is why ratio uses a wide intermediate.
func TestRatioDoesNotOverflowOnDurations(t *testing.T) {
	twoHundredDays := int64(200 * 24 * time.Hour)
	thirtyDays := int64(30 * 24 * time.Hour)
	if got := ratio(twoHundredDays, thirtyDays); got != BPS {
		t.Fatalf("ratio(200d, 30d) = %d, want %d (saturated)", got, BPS)
	}
	// The naive form really does wrap, so the guard is load-bearing.
	naive := twoHundredDays * BPS
	if naive > 0 && naive/BPS == twoHundredDays {
		t.Skip("int64 got wider; the overflow this guards no longer exists")
	}
	if got := ratio(math.MaxInt64, 1); got != BPS {
		t.Errorf("ratio(max, 1) = %d, want saturation", got)
	}
	if got := ratio(1, math.MaxInt64); got != 0 {
		t.Errorf("ratio(1, max) = %d, want 0", got)
	}
}

func TestRatioEdgeCases(t *testing.T) {
	for _, c := range []struct{ v, target, want int64 }{
		{0, 100, 0}, {-5, 100, 0}, {100, 0, 0}, {100, -1, 0},
		{50, 100, 5_000}, {100, 100, BPS}, {200, 100, BPS},
	} {
		if got := ratio(c.v, c.target); got != c.want {
			t.Errorf("ratio(%d,%d) = %d, want %d", c.v, c.target, got, c.want)
		}
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

// A list threshold above the lowest tier creates a dead zone: scores in between
// qualify for leverage they can never be granted.
func TestValidateCatchesTheDeadZone(t *testing.T) {
	c := DefaultConfig()
	c.ListAbove = c.Tiers[len(c.Tiers)-1].MinScore + 1_000
	if err := c.Validate(); err == nil {
		t.Fatal("a list threshold above the lowest tier must be rejected")
	}
}

func TestValidateCatchesBadWeightsAndOrdering(t *testing.T) {
	c := DefaultConfig()
	c.WeightDepth += 100
	if err := c.Validate(); err == nil {
		t.Error("weights that do not sum to BPS must be rejected")
	}

	c = DefaultConfig()
	c.DegradeBelow = c.ListAbove + 1
	if err := c.Validate(); err == nil {
		t.Error("a degrade threshold above the list threshold must be rejected")
	}

	c = DefaultConfig()
	c.DelistBelow = c.DegradeBelow + 1
	if err := c.Validate(); err == nil {
		t.Error("a delist threshold above the degrade threshold must be rejected")
	}
}

// Each cheap signal is a gate: necessary, never sufficient. A market failing
// one is refused however well it scores on everything else.
func TestEachGateIndependentlyRefusesAMarket(t *testing.T) {
	cfg := DefaultConfig()
	if s := Assess(baseObs(), cfg); !s.Eligible() {
		t.Fatalf("baseline should pass every gate, failed: %v", s.GateFailures)
	}

	cases := []struct {
		name string
		mut  func(*Observation)
	}{
		{"volume", func(o *Observation) { o.Volume24h = cfg.MinVolume24h - 1 }},
		{"tvl", func(o *Observation) { o.SpotTVL = cfg.MinSpotTVL - 1 }},
		{"age", func(o *Observation) { o.Age = cfg.MinAge - time.Minute }},
		{"concentration", func(o *Observation) { o.TopHolderShareBps = cfg.MaxTopHolderBps + 1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := baseObs()
			c.mut(&o)
			s := Assess(o, cfg)
			if s.Eligible() {
				t.Errorf("%s gate did not fire", c.name)
			}
			if _, ok := Derive(o, s, cfg); ok {
				t.Errorf("%s: derived a capacity for an ineligible market", c.name)
			}
		})
	}
}

// Concentration is a disqualifier, not a discount: no amount of depth
// compensates for one wallet being able to exit into the market.
func TestConcentrationIsNotOutweighedByDepth(t *testing.T) {
	cfg := DefaultConfig()
	o := baseObs()
	o.DepthWithin2Pct = 100_000_000 // absurdly deep
	o.TopHolderShareBps = 8_000
	s := Assess(o, cfg)
	if s.Eligible() {
		t.Fatal("a market with 80% of supply in one wallet must be refused at any depth")
	}
}

// Leverage comes out of the liquidity and the score only caps it.
//
// This test used to assert the reverse -- a score band mapped to a leverage
// number -- and that mapping is gone. It was the reason a market could not go
// above 5x however deep it was, and the reason a thin market got 2x when the
// arithmetic said it could not safely close a position at all.
func TestLeverageComesFromLiquidityAndIsCappedByScore(t *testing.T) {
	cfg := DefaultConfig()

	ceilingFor := func(score int64) int64 {
		for _, tr := range cfg.Tiers {
			if score >= tr.MinScore {
				return tr.Leverage
			}
		}
		return 0
	}

	// The same excellent score, on books an order of magnitude apart. The score
	// is doing nothing here; the book is doing everything.
	rich := baseObs()
	rich.DepthWithin2Pct = 20_000_000
	rich.UnderwrittenCapital = 2_000_000
	rich.OracleConfidence = 10_000
	rich.TopHolderShareBps = 100
	rich.TopPoolShareBps = 500
	rich.RealisedVolBps = 1_000

	var prev int64
	for _, book := range []int64{500_000, 2_000_000, 8_000_000} {
		o := rich
		o.BookDepthWithin2Pct = book
		sc := Assess(o, cfg)
		c, ok := Derive(o, sc, cfg)
		if !ok {
			t.Fatalf("book %d: offered nothing at score %d", book, sc.Total)
		}
		if c.MaxLeverage < prev {
			t.Errorf("a deeper book gave less leverage: %d -> %dx after %dx",
				book, c.MaxLeverage, prev)
		}
		if ceil := ceilingFor(sc.Total); c.MaxLeverage > ceil {
			t.Errorf("book %d gave %dx, above the %dx its score allows", book, c.MaxLeverage, ceil)
		}
		prev = c.MaxLeverage
	}
	if prev <= 5 {
		t.Errorf("a market with an 8M book topped out at %dx; leverage is still "+
			"coming from a table rather than from the liquidity", prev)
	}

	// And the score still binds: a market deep enough for high leverage but too
	// young, too concentrated or badly priced does not get it.
	shaky := rich
	shaky.BookDepthWithin2Pct = 8_000_000
	shaky.Age = 30 * time.Hour
	shaky.TopHolderShareBps = 4_000
	shaky.OracleConfidence = 6_000
	shaky.RealisedVolBps = 30_000
	ss := Assess(shaky, cfg)
	sc, ok := Derive(shaky, ss, cfg)
	if !ok {
		t.Fatalf("a deep but shaky market was refused outright at score %d", ss.Total)
	}
	if ceil := ceilingFor(ss.Total); sc.MaxLeverage > ceil {
		t.Errorf("a shaky market got %dx above its %dx ceiling", sc.MaxLeverage, ceil)
	}
	if sc.MaxLeverage >= prev {
		t.Errorf("a shaky market got %dx, no less than the sound one's %dx; "+
			"the score is not capping anything", sc.MaxLeverage, prev)
	}
}

// Passing every gate is not enough. A market that clears the cheap signals but
// scores below the lowest tier gets no leverage at all — which is the whole
// point of gates being necessary rather than sufficient.
func TestPassingGatesIsNotEnoughToBeListed(t *testing.T) {
	cfg := DefaultConfig()
	o := baseObs()
	o.DepthWithin2Pct = 300_000
	o.BookDepthWithin2Pct = 120_000
	o.UnderwrittenCapital = 120_000
	o.OracleConfidence = 6_000
	o.TopPoolShareBps = 7_000
	o.RealisedVolBps = 30_000

	s := Assess(o, cfg)
	if !s.Eligible() {
		t.Fatalf("this fixture should clear every gate, failed: %v", s.GateFailures)
	}
	if s.Total >= cfg.ListAbove {
		t.Fatalf("fixture drifted above the list threshold: %d", s.Total)
	}
	// Below the list threshold but above the spot rung: offerable, but at 1x
	// only. This used to be a cliff -- no tier matched, so a market with real
	// depth and every gate passing was offered nothing at all, which pushes
	// every holder toward the exit at once.
	c, ok := Derive(o, s, cfg)
	if !ok {
		t.Fatalf("a sound market scoring %d was offered nothing; the spot rung exists to catch it", s.Total)
	}
	if c.MaxLeverage != 1 {
		t.Errorf("a market below the list threshold got %dx, want 1x", c.MaxLeverage)
	}

	// And below the spot rung it is still a market, because the gates already
	// said so. The tiers grant leverage; they do not grant existence. Removing
	// a decaying market is the lifecycle's job, checked just below.
	o.OracleConfidence = 5_000
	o.UnderwrittenCapital = 0
	o.Age = 25 * time.Hour
	deep := Assess(o, cfg)
	if deep.Total >= cfg.Tiers[len(cfg.Tiers)-1].MinScore {
		t.Skipf("fixture scores %d, still above the spot rung", deep.Total)
	}
	c2, ok := Derive(o, deep, cfg)
	if !ok {
		t.Fatalf("a gate-passing market scoring %d was offered nothing", deep.Total)
	}
	if c2.MaxLeverage != 1 {
		t.Errorf("a market below the spot rung got %dx, want 1x", c2.MaxLeverage)
	}
}

// What actually removes a decaying market is the lifecycle, not the tier table.
//
// Derive answers "what may this lane do"; the machine answers "should this lane
// exist". Keeping the second question out of the first is what lets a market
// that clears every gate open at 1x without our balance sheet deciding whether
// the asset is real.
func TestTheLifecycleRemovesADecayedMarketNotTheTierTable(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	now := epoch

	good := baseObs()
	if Assess(good, cfg).Total < cfg.ListAbove {
		t.Fatal("fixture should list")
	}
	for now.Sub(epoch) <= cfg.ListSustained+time.Minute {
		good.Time = now
		m.Step(good, now)
		now = now.Add(time.Minute)
	}
	if m.State() != Live {
		t.Fatalf("state = %s, want live", m.State())
	}

	// Now let the asset itself decay past the delist threshold.
	bad := baseObs()
	bad.DepthWithin2Pct = 60_000
	bad.BookDepthWithin2Pct = 5_000
	bad.UnderwrittenCapital = 0
	bad.OracleConfidence = 1_000
	bad.TopPoolShareBps = 9_800
	bad.RealisedVolBps = 39_000
	if got := Assess(bad, cfg).Total; got >= cfg.DelistBelow {
		t.Fatalf("fixture scores %d, needs to sit below delist %d", got, cfg.DelistBelow)
	}
	deadline := now.Add(cfg.DelistSustained + time.Hour)
	for now.Before(deadline) && m.State() != ReduceOnly {
		bad.Time = now
		m.Step(bad, now)
		now = now.Add(time.Minute)
	}
	if m.State() != ReduceOnly {
		t.Fatalf("a decayed market stayed %s; the lifecycle must close it to new exposure", m.State())
	}
}

// The cap answers "how much can we actually close or cover", so it comes from
// the book and from first-loss capital — never from the score.
func TestOICapComesFromLiquidityNotScore(t *testing.T) {
	cfg := DefaultConfig()
	o := baseObs()
	c, _ := Derive(o, Assess(o, cfg), cfg)

	// From *our* book, not the venue's: a forced close matches here.
	fromDepth := o.BookDepthWithin2Pct * cfg.OIFractionOfDepthBps / BPS
	fromCapital := o.UnderwrittenCapital * cfg.UnderwritingMultipleBps / BPS
	if c.OICap != fromDepth+fromCapital {
		t.Errorf("OI cap = %d, want %d (book) + %d (underwriting)",
			c.OICap, fromDepth, fromCapital)
	}

	// Doubling our book doubles the book component, leaving capital alone.
	o.BookDepthWithin2Pct *= 2
	c2, _ := Derive(o, Assess(o, cfg), cfg)
	if c2.OICap != fromDepth*2+fromCapital {
		t.Errorf("doubling the book gave %d, want %d", c2.OICap, fromDepth*2+fromCapital)
	}

	// And venue depth alone does not move it. The AMM being deep says the
	// underlying is tradeable; it does not say this lane can close a position.
	o = baseObs()
	o.DepthWithin2Pct *= 10
	c3, _ := Derive(o, Assess(o, cfg), cfg)
	base, _ := Derive(baseObs(), Assess(baseObs(), cfg), cfg)
	if c3.OICap != base.OICap {
		t.Errorf("ten times the venue depth changed the cap: %d vs %d; "+
			"capacity must come from the book a liquidation eats", c3.OICap, base.OICap)
	}

	// The score does not enter into it: a much better market with the same
	// liquidity gets the same cap.
	better := baseObs()
	better.OracleConfidence = 10_000
	better.TopHolderShareBps = 10
	if Assess(better, cfg).Total <= Assess(baseObs(), cfg).Total {
		t.Fatal("fixture should score higher")
	}
	cBetter, _ := Derive(better, Assess(better, cfg), cfg)
	if cBetter.OICap != c.OICap {
		t.Errorf("a higher score changed the cap: %d vs %d", c.OICap, cBetter.OICap)
	}
}

// Maintenance must cover what liquidating actually costs, or every liquidation
// leaves a hole by construction. This mirrors the VM's own invariant.
func TestMaintenanceCoversLiquidationCost(t *testing.T) {
	cfg := DefaultConfig()
	o := baseObs()
	s := Assess(o, cfg)
	c, ok := Derive(o, s, cfg)
	if !ok {
		t.Fatal("expected a capacity")
	}
	if c.MaintenanceBps <= cfg.LiquidationFeeBps {
		t.Errorf("maintenance %d must exceed the liquidation fee %d",
			c.MaintenanceBps, cfg.LiquidationFeeBps)
	}
	if c.InitialBps <= c.MaintenanceBps {
		t.Errorf("initial %d must exceed maintenance %d, or positions open liquidatable",
			c.InitialBps, c.MaintenanceBps)
	}
}

func TestUninitialisedCapacityRefusesToEmitParams(t *testing.T) {
	var empty Capacity
	base := DefaultConfig()
	_ = base
	if _, err := empty.RiskParams(wireBase(), markBand()); err == nil {
		t.Fatal("a zero capacity must not emit params: a zero initial margin reads as infinite leverage")
	}

	bad := Capacity{Openable: true, MaxLeverage: 2, InitialBps: 100, MaintenanceBps: 300}
	if _, err := bad.RiskParams(wireBase(), markBand()); err == nil {
		t.Fatal("initial margin below maintenance must be refused")
	}
}

// A market that cannot be granted leverage must not have its margin
// requirements zeroed. It keeps them and stops opening.
func TestFailedDerivationDoesNotZeroTheMargins(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)

	good := baseObs()
	now := epoch
	for i := 0; i < 5; i++ {
		good.Time = now
		m.Step(good, now)
		now = now.Add(15 * time.Minute)
	}
	if m.State() != Live {
		t.Fatalf("expected the market to list, got %s", m.State())
	}
	before := m.Capacity()
	if before.InitialBps == 0 {
		t.Fatal("expected a real initial margin")
	}

	// Collapse it so Derive fails.
	bad := good
	bad.Volume24h = 0 // a hard gate: there is no market here at all
	bad.DepthWithin2Pct = 1_000
	for i := 0; i < 4; i++ {
		bad.Time = now
		m.Step(bad, now)
		now = now.Add(time.Hour)
	}

	after := m.Capacity()
	if after.InitialBps == 0 || after.MaintenanceBps == 0 {
		t.Fatalf("margins were zeroed on a failed derivation: %+v", after)
	}
	if after.Openable {
		t.Error("a market that cannot be scored must not be openable")
	}
}

// / Underwriting capital creates real capacity, which is what makes rent worth
// / paying and underwriting worth doing: the flywheel needs the capital to
// / actually buy room, not merely to score well.
func TestUnderwritingCapitalExtendsTheOICap(t *testing.T) {
	cfg := DefaultConfig()
	bare := baseObs()
	bare.UnderwrittenCapital = 0
	backed := baseObs()
	backed.UnderwrittenCapital = 500_000

	cBare, ok := Derive(bare, Assess(bare, cfg), cfg)
	if !ok {
		t.Fatal("expected a capacity without underwriting")
	}
	cBacked, ok := Derive(backed, Assess(backed, cfg), cfg)
	if !ok {
		t.Fatal("expected a capacity with underwriting")
	}

	if cBacked.OICap <= cBare.OICap {
		t.Errorf("underwriting did not extend capacity: %d vs %d",
			cBare.OICap, cBacked.OICap)
	}
	want := cBare.OICap + 500_000*cfg.UnderwritingMultipleBps/BPS
	if cBacked.OICap != want {
		t.Errorf("OI cap = %d, want %d", cBacked.OICap, want)
	}
}

// / But capital does not fill orders. Closing still has to eat the book, so
// / maintenance margin must keep tracking depth rather than total capacity.
func TestUnderwritingDoesNotReduceTheSlippageRequirement(t *testing.T) {
	cfg := DefaultConfig()
	backed := baseObs()
	backed.UnderwrittenCapital = 2_000_000

	c, ok := Derive(backed, Assess(backed, cfg), cfg)
	if !ok {
		t.Fatal("expected a capacity")
	}
	if c.MaintenanceBps <= cfg.LiquidationFeeBps {
		t.Error("maintenance must still cover the cost of closing against the book")
	}
	if c.InitialBps <= c.MaintenanceBps {
		t.Errorf("initial %d must exceed maintenance %d", c.InitialBps, c.MaintenanceBps)
	}
}

// Our own book counts toward the depth subscore, and the venue's does not
// replace it. Regression for a market being scored on the thinness of a pool
// it does not trade on: the same conflation the gates had, one level down.
func TestOurOwnBookCountsTowardDepth(t *testing.T) {
	cfg := DefaultConfig()
	base := Observation{
		DepthWithin2Pct: 15_000, UnderwrittenCapital: 400_000,
		Age: 200 * 24 * time.Hour, TopHolderShareBps: 1500, TopPoolShareBps: 9000,
		OracleConfidence: 9_000, Volume24h: 2_000_000, SpotTVL: 400_000,
		MarketCap: 20_000_000, RealisedVolBps: 15_000,
	}
	thin := Assess(base, cfg)

	withBook := base
	withBook.BookDepthWithin2Pct = 800_000
	deep := Assess(withBook, cfg)

	if deep.Sub.Depth <= thin.Sub.Depth {
		t.Fatalf("an $800k book of our own did not raise the depth subscore: %d -> %d",
			thin.Sub.Depth, deep.Sub.Depth)
	}
	if deep.Total <= thin.Total {
		t.Fatalf("our own book did not raise the total: %d -> %d", thin.Total, deep.Total)
	}
}

// A market with no book of ours is still scored on the venue, or nothing would
// ever list: at bootstrap the venue is the only evidence there is.
func TestAnEmptyBookFallsBackToTheVenue(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		DepthWithin2Pct: 4_000_000, BookDepthWithin2Pct: 0,
		UnderwrittenCapital: 250_000, Age: 200 * 24 * time.Hour,
		TopHolderShareBps: 700, TopPoolShareBps: 4_000,
		OracleConfidence: 9_200, Volume24h: 20_000_000, SpotTVL: 9_000_000,
		MarketCap: 120_000_000, RealisedVolBps: 9_000,
	}
	s := Assess(o, cfg)
	if s.Sub.Depth == 0 {
		t.Fatal("a deep venue with no book of ours scored zero depth: nothing would ever list")
	}
	if !s.Eligible() {
		t.Fatalf("cold start refused: %v", s.GateFailures)
	}
}

// An empty book of our own withdraws leverage; it does not withdraw the market.
//
// Listing and lending are bounded by different things. Closable capacity --
// book plus first-loss capital -- is what a forced close eats and what absorbs
// a loss the book could not clear. A fully collateralised position has neither
// problem: the trader's own margin covers the whole notional. So a market with
// deep spot, real volume and no book of ours is listable at 1x, and whether any
// particular order fills is the matching engine's business at trade time.
//
// This also pins the inversion that exposed the confusion: $500k underwritten
// with no book must not be refused while the same lane with nothing underwritten
// lists. More capital can never mean fewer markets.
func TestAnEmptyBookWithdrawsLeverageNotTheMarket(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		DepthWithin2Pct: 8_900_000, BookDepthWithin2Pct: 0,
		UnderwrittenCapital: 0, Age: 60 * 24 * time.Hour,
		TopHolderShareBps: 1000, TopPoolShareBps: 3000,
		OracleConfidence: 9_000, Volume24h: 800_000_000,
		SpotTVL: 30_000_000, MarketCap: 500_000_000, RealisedVolBps: 5_000,
	}
	s := Assess(o, cfg)
	if !s.Eligible() {
		t.Fatalf("fixture should clear every gate: %v", s.GateFailures)
	}
	c, ok := Derive(o, s, cfg)
	if !ok {
		t.Fatal("refused a listable spot market for having no book of our own")
	}
	if c.MaxLeverage != 1 {
		t.Fatalf("lent %dx against a market with no book to close in", c.MaxLeverage)
	}
	if c.OICap <= 0 {
		t.Fatal("listed a market that may carry no open interest")
	}
	// Bounded by what it costs to fake the mark, not by capital we have not put up.
	if want := manipulationCap(o.DepthWithin2Pct, cfg); c.OICap != want {
		t.Fatalf("spot OI cap %d, expected the manipulation bound %d", c.OICap, want)
	}

	// Capital and book only ever add.
	prev := int64(0)
	for _, tc := range []struct {
		under, book int64
	}{{0, 0}, {500_000, 0}, {0, 400_000}, {500_000, 400_000}} {
		o2 := o
		o2.UnderwrittenCapital, o2.BookDepthWithin2Pct = tc.under, tc.book
		s2 := Assess(o2, cfg)
		c2, ok2 := Derive(o2, s2, cfg)
		if !ok2 {
			t.Fatalf("refused at underwriting %d, book %d", tc.under, tc.book)
		}
		if c2.MaxLeverage < prev {
			t.Fatalf("leverage fell from %dx to %dx when capital rose (underwriting %d, book %d)",
				prev, c2.MaxLeverage, tc.under, tc.book)
		}
		prev = c2.MaxLeverage
	}
}

// A spot market must be tradeable. The margin slope pushes a fully
// collateralised rate above 100%, which is correct and safe; integer leverage
// can only render that as zero, and zero was being read as "not offerable".
//
// Every market on Robinhood Chain is on the spot rung, so this refused every
// order on the exchange.
func TestASpotMarketIsOfferableAtEverySize(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		DepthWithin2Pct: 2_000_000, BookDepthWithin2Pct: 15_000,
		UnderwrittenCapital: 0, Age: 60 * 24 * time.Hour,
		TopHolderShareBps: 1000, TopPoolShareBps: 4000,
		OracleConfidence: 8_000, Volume24h: 800_000_000,
		SpotTVL: 30_000_000, MarketCap: 500_000_000, RealisedVolBps: 5_000,
	}
	s := Assess(o, cfg)
	c, ok := Derive(o, s, cfg)
	if !ok {
		t.Fatalf("fixture should list: %v %v", s.GateFailures, s.LeverageBlockers)
	}
	if c.MaxLeverage != 1 {
		t.Fatalf("fixture should be on the spot rung, got %dx", c.MaxLeverage)
	}

	for _, notional := range []int64{25, 2_500, 25_000, 250_000} {
		lev := c.LeverageAt(notional)
		margin := c.MarginAt(notional)
		if notional > c.MaxPosition {
			continue // refused for size, which is a different thing
		}
		if lev < 1 {
			t.Fatalf("notional %d reported %dx: a payable size read as unofferable",
				notional, lev)
		}
		// And the margin must cover it: at least the notional, since the rate
		// is at or above 100% here.
		if margin < notional {
			t.Fatalf("notional %d wants only %d margin on a 1x market", notional, margin)
		}
	}
}

// Margin is the quantity that keeps meaning something as size grows, so it
// must rise even where leverage has flattened to its floor.
func TestMarginRisesWithSizeWhereLeverageCannot(t *testing.T) {
	c := Capacity{InitialBps: BPS, MarginSlopePerUnit: 1_000_000_000, MaxPosition: 1 << 40}
	small, large := c.MarginAt(1_000), c.MarginAt(100_000)
	if c.LeverageAt(1_000) != 1 || c.LeverageAt(100_000) != 1 {
		t.Fatal("fixture should sit at the leverage floor for both")
	}
	if !(large > small*50) {
		t.Fatalf("margin %d then %d: the slope is not reaching the figure traders see",
			small, large)
	}
}
