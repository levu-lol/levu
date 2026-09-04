package health

import (
	"testing"
	"time"

	"github.com/levu-lol/levu/wire"
)

// step drives the machine for a stretch, returning every transition emitted.
func step(m *Machine, o Observation, from time.Time, n int, every time.Duration) ([]Transition, time.Time) {
	var out []Transition
	now := from
	for i := 0; i < n; i++ {
		o.Time = now
		if tr := m.Step(o, now); tr != nil {
			out = append(out, *tr)
		}
		now = now.Add(every)
	}
	return out, now
}

// The rung this whole change exists for. A market with real depth, passing
// every gate, that has simply gone quiet, must be offered 1x — not nothing.
func TestAFadedMarketGetsSpotNotACliff(t *testing.T) {
	cfg := DefaultConfig()
	o := fadedButSound()
	s := Assess(o, cfg)
	if !s.Eligible() {
		t.Fatalf("fixture should pass every gate, failed: %v", s.GateFailures)
	}
	if s.Total >= cfg.ListAbove {
		t.Fatalf("fixture scores %d, above the list threshold", s.Total)
	}
	c, ok := Derive(o, s, cfg)
	if !ok {
		t.Fatalf("a market scoring %d with %d depth was offered nothing", s.Total, o.DepthWithin2Pct)
	}
	// Capped by what this score band is allowed, rather than by a constant.
	// The number offered comes out of the liquidity now, so asserting a literal
	// here would break every time the curve is retuned and would be testing the
	// tuning rather than the rule.
	ceiling := int64(0)
	for _, tr := range cfg.Tiers {
		if s.Total >= tr.MinScore {
			ceiling = tr.Leverage
			break
		}
	}
	if ceiling == 0 {
		t.Fatal("a listable score matched no tier")
	}
	if c.MaxLeverage > ceiling {
		t.Errorf("got %dx above the %dx ceiling for score %d", c.MaxLeverage, ceiling, s.Total)
	}
	if c.MaxLeverage < 1 {
		t.Errorf("got %dx", c.MaxLeverage)
	}

	// And further down, into the spot band, it is 1x rather than nothing.
	sp := spotBand()
	ss := Assess(sp, cfg)
	sc, sok := Derive(sp, ss, cfg)
	if !sok {
		t.Fatalf("a market scoring %d with %d depth was offered nothing; "+
			"this is the cliff the spot rung exists to remove", ss.Total, sp.DepthWithin2Pct)
	}
	if sc.MaxLeverage != 1 {
		t.Errorf("spot band got %dx, want 1x", sc.MaxLeverage)
	}
	c = sc
	if !c.Openable {
		t.Error("spot-only must still be openable; that is the whole point")
	}
	if c.InitialBps != BPS {
		t.Errorf("1x must be fully collateralised: initial margin %d bps, want %d", c.InitialBps, BPS)
	}
}

// The full ladder, down and back up.
func TestTheLadderGoesDownThroughSpotAndBackUp(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	now := t0

	healthy := obsAt(0, 4_000_000)
	trs, now := step(m, healthy, now, 40, 5*time.Minute)
	if m.State() != Live {
		t.Fatalf("a healthy market is %s, want live", m.State())
	}
	if m.Capacity().MaxLeverage < 3 {
		t.Fatalf("a healthy market got %dx", m.Capacity().MaxLeverage)
	}

	// It fades: still sound, just uninteresting.
	trs, now = step(m, spotBand(), now, 60, 5*time.Minute)
	if m.State() != SpotOnly {
		t.Fatalf("a faded market is %s, want spot-only. transitions: %v", m.State(), kinds(trs))
	}
	if lev := m.Capacity().MaxLeverage; lev != 1 {
		t.Errorf("spot-only offers %dx, want 1x", lev)
	}
	if !m.Capacity().Openable {
		t.Error("spot-only stopped accepting positions; that is reduce-only, not spot")
	}

	// Interest returns.
	trs, _ = step(m, healthy, now, 40, 5*time.Minute)
	if !m.State().Openable() {
		t.Fatalf("a recovered market is %s", m.State())
	}
	if m.State() == SpotOnly {
		t.Errorf("a recovered market never earned leverage back: %v", kinds(trs))
	}
	if m.Capacity().MaxLeverage < 2 {
		t.Errorf("recovered to %dx", m.Capacity().MaxLeverage)
	}
}

func kinds(trs []Transition) []string {
	out := make([]string, len(trs))
	for i, tr := range trs {
		out[i] = tr.From.String() + "->" + tr.To.String()
	}
	return out
}

// The state caps what the tier proposes, so a score bouncing for one
// observation cannot walk leverage back without a transition.
func TestTheStateCapsWhatTheTierProposes(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	m.state = SpotOnly
	// A tier that would happily give 5x.
	rich := obsAt(0, 8_000_000)
	for i := 0; i < 5; i++ {
		rich.Time = t0.Add(time.Duration(i) * time.Minute)
		m.Step(rich, rich.Time)
	}
	if lev := m.Capacity().MaxLeverage; lev != 1 {
		t.Fatalf("spot-only handed out %dx on a high score; the ceiling did not bind", lev)
	}

	m2 := NewMachine(cfg)
	m2.state = Degraded
	for i := 0; i < 5; i++ {
		rich.Time = t0.Add(time.Duration(i) * time.Minute)
		m2.Step(rich, rich.Time)
	}
	if lev := m2.Capacity().MaxLeverage; lev != 2 {
		t.Fatalf("degraded handed out %dx, want the 2x ceiling", lev)
	}
}

// Spot-only recovers; reduce-only does not. The asymmetry is deliberate: one
// is a downgrade, the other is how a drained market is stopped.
func TestSpotOnlyRecoversButReduceOnlyDoesNot(t *testing.T) {
	cfg := DefaultConfig()
	healthy := obsAt(0, 4_000_000)

	spot := NewMachine(cfg)
	spot.state = SpotOnly
	if _, _ = step(spot, healthy, t0, 40, 5*time.Minute); spot.State() == SpotOnly {
		t.Error("spot-only never recovered on sustained good readings")
	}

	reduce := NewMachine(cfg)
	reduce.state = ReduceOnly
	if _, _ = step(reduce, healthy, t0, 40, 5*time.Minute); reduce.State() != ReduceOnly {
		t.Errorf("reduce-only recovered to %s; a drained market must not re-open itself",
			reduce.State())
	}
}

// A collapse must not be held at 2x while a second timer runs.
func TestACollapsingMarketFallsPastDegraded(t *testing.T) {
	cfg := DefaultConfig()
	m := NewMachine(cfg)
	now := t0
	_, now = step(m, obsAt(0, 4_000_000), now, 40, 5*time.Minute)
	if m.State() != Live {
		t.Fatalf("setup: state is %s", m.State())
	}
	trs, _ := step(m, spotBand(), now, 20, 5*time.Minute)

	for _, tr := range trs {
		if tr.From == Live && tr.To == SpotOnly {
			return // fell straight through, as intended
		}
	}
	if m.State() != SpotOnly && m.State() != ReduceOnly {
		t.Fatalf("a collapsing market ended in %s: %v", m.State(), kinds(trs))
	}
}

func TestValidateRefusesALadderWithNoSpotRung(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Tiers = []Tier{{MinScore: 8_500, Leverage: 5}, {MinScore: 5_500, Leverage: 2}}
	if err := cfg.Validate(); err == nil {
		t.Error("accepted a ladder whose bottom rung is 2x, which drops a fading market off a cliff")
	}

	cfg = DefaultConfig()
	cfg.SpotOnlyBelow = cfg.DelistBelow - 100
	if err := cfg.Validate(); err == nil {
		t.Error("accepted a config that loses the market before the leverage")
	}
}

// A tier table can promise leverage the arithmetic cannot deliver, and the
// place that mistake surfaces should be here rather than on a marketing page.
func TestTheTiersDoNotPromiseMoreThanTheEngineCanDeliver(t *testing.T) {
	cfg := DefaultConfig()
	max := AbsoluteMaxLeverage(cfg)
	if max <= 1 {
		t.Fatalf("this configuration cannot offer leverage at all: %dx", max)
	}
	for _, tr := range cfg.Tiers {
		if tr.Leverage > max {
			t.Errorf("a tier advertises %dx but the engine tops out at %dx: "+
				"maintenance cannot go below the liquidation fee plus %dbps, so "+
				"nothing can reach it", tr.Leverage, max, vmLiquidationBufferBps)
		}
	}

	// And the ceiling really is the VM's constraint, not a config choice.
	free := cfg
	free.LiquidationFeeBps, free.MarkBandBps, free.MinMaintenanceBps = 0, 1, 1
	free.MaxLeverage = 100_000
	if got, want := AbsoluteMaxLeverage(free), BPS/(2*vmLiquidationBufferBps); got != want {
		t.Errorf("with every fee at zero the ceiling is %dx, want %dx", got, want)
	}
}

// A book can be too small to lever against at all, whatever the market scores.
//
// Smooth degradation is not enough on its own. Before the floor, a market whose
// entire book was $25k was offered 7x on positions of $76k -- three times the
// whole book -- at a maintenance margin of 7.1%. That position cannot be closed
// at any price, and the linear slippage model quoted a finite cost for it
// because the model stops being true long before it stops returning a number.
func TestABookTooThinToLeverAgainstGetsSpotOnly(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MinBookForLeverage <= 0 {
		t.Fatal("no depth floor configured")
	}
	base := Observation{
		DepthWithin2Pct: 5_000_000, UnderwrittenCapital: 250_000,
		Age: 200 * 24 * time.Hour, TopHolderShareBps: 600, TopPoolShareBps: 3_000,
		OracleConfidence: 9_000, Volume24h: 30_000_000, SpotTVL: 10_000_000,
		MarketCap: 200_000_000, RealisedVolBps: 6_000,
	}

	thin := base
	thin.BookDepthWithin2Pct = cfg.MinBookForLeverage - 1
	s := Assess(thin, cfg)
	c, ok := Derive(thin, s, cfg)
	if !ok {
		t.Fatalf("a thin market was refused outright; it should trade at 1x (score %d)", s.Total)
	}
	if c.MaxLeverage != 1 {
		t.Errorf("a book below the floor got %dx, want 1x", c.MaxLeverage)
	}
	if !c.Openable {
		t.Error("spot-only must still be openable")
	}

	// One unit over the floor, the same market may borrow.
	ok_ := base
	ok_.BookDepthWithin2Pct = cfg.MinBookForLeverage
	c2, _ := Derive(ok_, Assess(ok_, cfg), cfg)
	if c2.MaxLeverage <= 1 {
		t.Errorf("a book at the floor still got %dx", c2.MaxLeverage)
	}
}

// However the OI cap is inflated by underwriting, a single position may never
// be large relative to the book it has to be closed in.
func TestNoPositionIsEverLargerThanTheBookCanClose(t *testing.T) {
	cfg := DefaultConfig()
	base := Observation{
		DepthWithin2Pct: 5_000_000,
		// Underwriting far larger than the book: capital absorbs loss, it does
		// not fill orders, and it used to inflate position size regardless.
		UnderwrittenCapital: 5_000_000,
		Age:                 200 * 24 * time.Hour, TopHolderShareBps: 600,
		TopPoolShareBps: 3_000, OracleConfidence: 9_000, Volume24h: 30_000_000,
		SpotTVL: 10_000_000, MarketCap: 200_000_000, RealisedVolBps: 6_000,
	}
	for _, book := range []int64{300_000, 1_000_000, 10_000_000} {
		o := base
		o.BookDepthWithin2Pct = book
		c, ok := Derive(o, Assess(o, cfg), cfg)
		if !ok {
			t.Fatalf("book %d refused", book)
		}
		if c.MaxPosition <= 0 {
			t.Fatalf("book %d: no max position reported", book)
		}
		share := c.MaxPosition * BPS / book
		if share > cfg.MaxPositionOfBookBps {
			t.Errorf("book %d allows a position of %d, %d bps of the book, "+
				"above the %d bps cap; closing it is not a slippage question",
				book, c.MaxPosition, share, cfg.MaxPositionOfBookBps)
		}
	}
}

// What a trader asks for and what the book can carry are different numbers, and
// the difference grows with their size.
func TestLeverageFallsWithTheSizeAsked(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		DepthWithin2Pct: 20_000_000, BookDepthWithin2Pct: 4_000_000,
		UnderwrittenCapital: 1_000_000, Age: 400 * 24 * time.Hour,
		TopHolderShareBps: 400, TopPoolShareBps: 2_500, OracleConfidence: 9_800,
		Volume24h: 100_000_000, SpotTVL: 50_000_000, MarketCap: 2_000_000_000,
		RealisedVolBps: 4_000,
	}
	c, ok := Derive(o, Assess(o, cfg), cfg)
	if !ok {
		t.Fatal("market refused")
	}
	if c.MarginSlopePerUnit <= 0 {
		t.Fatal("no slope derived; every size would get the headline number")
	}

	small := c.LeverageAt(1_000)
	large := c.LeverageAt(1_000_000)
	if small != c.MaxLeverage {
		t.Errorf("a tiny position got %dx against a headline of %dx", small, c.MaxLeverage)
	}
	if large >= small {
		t.Errorf("a position a thousand times larger got %dx, no less than %dx; "+
			"the slope is not reaching the trader", large, small)
	}
	if large < 1 {
		t.Errorf("leverage collapsed to %dx", large)
	}

	// Monotone: every step up in size is a step down in leverage, never a jump
	// back up. A trader sizing into a position must not find that asking for
	// more improves their terms.
	prev := int64(1 << 30)
	for _, n := range []int64{1_000, 10_000, 100_000, 500_000, 1_000_000, 5_000_000} {
		lev := c.LeverageAt(n)
		if lev > prev {
			t.Errorf("size %d got %dx, more than the %dx at a smaller size", n, lev, prev)
		}
		prev = lev
	}
}

// The slope has to reach the VM, or the engine computes a number nobody enforces.
func TestTheSlopeIsEmittedToTheVM(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		DepthWithin2Pct: 20_000_000, BookDepthWithin2Pct: 4_000_000,
		UnderwrittenCapital: 1_000_000, Age: 400 * 24 * time.Hour,
		TopHolderShareBps: 400, TopPoolShareBps: 2_500, OracleConfidence: 9_800,
		Volume24h: 100_000_000, SpotTVL: 50_000_000, MarketCap: 2_000_000_000,
		RealisedVolBps: 4_000,
	}
	c, _ := Derive(o, Assess(o, cfg), cfg)
	p, err := c.RiskParams(wire.ConservativeParams(), wire.FixedRawInt64(5_000_000_000_000_000))
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	if p.MarginSlope.IsZero() {
		t.Fatal("the slope was computed and then not emitted; the VM would " +
			"charge every position the same rate whatever the engine decided")
	}
}

// Liquidity on the chain is not liquidity with us, and the gates used to
// confuse the two.
//
// Capacity comes from our own book; the venue we price against is a separate
// question and bounds how far the mark can be moved, not whether the market
// should exist. A market with a thin pool and a real book of our own was being
// refused for the thinness of somebody else's liquidity.
func TestAThinVenueWithdrawsLeverageNotTheMarket(t *testing.T) {
	cfg := DefaultConfig()
	o := Observation{
		// Measured on Robinhood Chain: PONS/USDG has one 1% pool.
		DepthWithin2Pct: 15_474,
		// And a book of our own, which is what a liquidation would eat.
		BookDepthWithin2Pct: 800_000,
		UnderwrittenCapital: 250_000,
		Age:                 1168 * time.Hour,
		TopHolderShareBps:   1_500,
		TopPoolShareBps:     10_000,
		OracleConfidence:    2,
		Volume24h:           5_000_000,
		SpotTVL:             760_000,
		MarketCap:           50_000_000,
		RealisedVolBps:      13_400,
	}
	s := Assess(o, cfg)
	if !s.Eligible() {
		t.Fatalf("a real market was gated out for the venue's thinness: %v", s.GateFailures)
	}
	if s.Leverable() {
		t.Error("a $15k venue and a single price source should block leverage")
	}
	for _, b := range s.LeverageBlockers {
		if b == "" {
			t.Error("a blocker with no reason")
		}
	}

	// The venue being deep changes the leverage question and nothing else.
	deep := o
	deep.DepthWithin2Pct = 5_000_000
	deep.OracleConfidence = 9_000
	if !Assess(deep, cfg).Leverable() {
		t.Error("a deep, well-covered venue still blocks leverage")
	}
}

// The two questions must not be able to collapse back into one.
func TestGatesAndLeverageBlockersStaySeparate(t *testing.T) {
	cfg := DefaultConfig()
	base := Observation{
		DepthWithin2Pct: 5_000_000, BookDepthWithin2Pct: 2_000_000,
		UnderwrittenCapital: 500_000, Age: 200 * 24 * time.Hour,
		TopHolderShareBps: 600, TopPoolShareBps: 3_000, OracleConfidence: 9_000,
		Volume24h: 30_000_000, SpotTVL: 10_000_000, MarketCap: 200_000_000,
		RealisedVolBps: 6_000,
	}
	if s := Assess(base, cfg); !s.Eligible() || !s.Leverable() {
		t.Fatalf("the baseline is not clean: %v %v", s.GateFailures, s.LeverageBlockers)
	}

	// No asset here: a gate, and no market at any leverage.
	for _, mut := range []func(*Observation){
		func(o *Observation) { o.Volume24h = 0 },
		func(o *Observation) { o.SpotTVL = 0 },
		func(o *Observation) { o.Age = time.Hour },
		func(o *Observation) { o.TopHolderShareBps = cfg.MaxTopHolderBps + 1 },
	} {
		o := base
		mut(&o)
		if Assess(o, cfg).Eligible() {
			t.Error("a market with no asset behind it passed the gates")
		}
	}

	// A real asset we cannot safely lend against: tradeable, at 1x.
	for _, mut := range []func(*Observation){
		func(o *Observation) { o.DepthWithin2Pct = cfg.MinDepth - 1 },
		func(o *Observation) { o.OracleConfidence = cfg.MinConfidence - 1 },
	} {
		o := base
		mut(&o)
		s := Assess(o, cfg)
		if !s.Eligible() {
			t.Errorf("a leverage blocker gated the market out entirely: %v", s.GateFailures)
		}
		if s.Leverable() {
			t.Error("the blocker did not fire")
		}
		c, ok := Derive(o, s, cfg)
		if !ok {
			t.Fatalf("a tradeable market was offered nothing (score %d)", s.Total)
		}
		if c.MaxLeverage != 1 {
			t.Errorf("a market we cannot price confidently got %dx", c.MaxLeverage)
		}
		if !c.Openable {
			t.Error("spot-only must still accept positions")
		}
	}
}
