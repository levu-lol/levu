package indexer

import (
	"math/big"
	"testing"
)

func bi(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }

// Real state from Robinhood Chain's PONS/WETH 1% pool, where the
// constant-liquidity estimate reported 156x the pool's entire balance.
func ponsWeth() (V3Pool, *V3State) {
	p := V3Pool{
		Symbol: "PONS", Decimals0: 18, Decimals1: 18,
		QuoteIsToken0: true, // WETH sorts below PONS
	}
	s := &V3State{
		SqrtPriceX96: bi("5251395821215709147186265889424"),
		Liquidity:    bi("224374890532394088223728"),
		Balance0:     bi("1266924868404724399197"),    // 1,266.92 WETH
		Balance1:     bi("5260615179230606231273697"), // 5,260,615 PONS
	}
	return p, s
}

// Depth is a claim about tokens the pool would have to pay out, so it is capped
// by the reserves that would pay it.
//
// Two-sided depth is buying *and* selling, and those drain opposite reserves:
// buying takes base out, selling takes quote out. So the bound on the sum is
// the whole pool, not either side of it -- which is what TestDepthNeverExceedsTVL
// checks. What this one checks is the part that was actually broken: an
// individual side can no longer exceed the reserve it is paid from.
func TestNeitherSideExceedsTheReserveThatPaysIt(t *testing.T) {
	p, s := ponsWeth()
	depth := p.DepthWithin2Pct(s)

	quoteBal := scaleDown(s.Balance0, 18) // WETH is the quote here
	// Before the bound this returned 198,261 against 1,266 of quote: a single
	// side alone was 156x the entire reserve.
	if depth.Cmp(new(big.Int).Mul(quoteBal, big.NewInt(2))) > 0 {
		t.Fatalf("depth %s is more than both reserves could pay (quote side %s)",
			depth, quoteBal)
	}
	if depth.Sign() <= 0 {
		t.Fatalf("depth collapsed to %s; the bound must cap, not zero", depth)
	}
	t.Logf("bounded depth %s WETH, quote reserve %s WETH (was 198,261 before)", depth, quoteBal)
}

// The haircut still applies on top of the reserve bound. The bound says what is
// physically possible; the haircut says how much of it is realistically
// reachable inside the band.
func TestTheHaircutStillAppliesOnTopOfTheBound(t *testing.T) {
	p, s := ponsWeth()
	bare := p.DepthWithin2Pct(s)
	p.DepthHaircutBps = 3000
	cut := p.DepthWithin2Pct(s)
	if cut.Cmp(bare) >= 0 {
		t.Fatalf("haircut did not reduce depth: %s vs %s", cut, bare)
	}
	want := new(big.Int).Quo(new(big.Int).Mul(bare, big.NewInt(7)), big.NewInt(10))
	if diff := new(big.Int).Sub(cut, want); diff.CmpAbs(big.NewInt(2)) > 0 {
		t.Errorf("haircut gave %s, want about %s", cut, want)
	}
}

// And it must not exceed the whole pool either way round.
func TestDepthNeverExceedsTVL(t *testing.T) {
	p, s := ponsWeth()
	if d, tvl := p.DepthWithin2Pct(s), p.TVL(s); d.Cmp(tvl) > 0 {
		t.Fatalf("depth %s exceeds TVL %s", d, tvl)
	}
}

// The bound must not fire on a pool whose liquidity really is spread out: a
// cap that always binds has replaced one wrong number with another.
func TestAWideRangePoolIsNotCappedAway(t *testing.T) {
	// WETH/USDG 0.01%, where the estimate was already below the reserves.
	p := V3Pool{Symbol: "WETH", Decimals0: 18, Decimals1: 6, QuoteIsToken0: false}
	s := &V3State{
		SqrtPriceX96: bi("4339505179874779672736474"),
		Liquidity:    bi("4517012579790668412"),
		Balance0:     bi("4009806889799196464247"), // 4,009 WETH
		Balance1:     bi("12328468292877"),         // 12.33M USDG
	}
	depth := p.DepthWithin2Pct(s)
	quoteBal := scaleDown(s.Balance1, 6)
	if depth.Sign() <= 0 {
		t.Fatal("a deep pool reported no depth")
	}
	if depth.Cmp(quoteBal) > 0 {
		t.Fatalf("depth %s still exceeds balance %s", depth, quoteBal)
	}
	t.Logf("depth %s USDG against a %s USDG balance", depth, quoteBal)
}

// Missing balances must not read as unlimited depth. The oracle path fetches
// two values rather than four, so a caller can arrive here with nil reserves.
func TestAbsentBalancesGiveNoDepth(t *testing.T) {
	p, s := ponsWeth()
	s.Balance0, s.Balance1 = nil, nil
	if d := p.DepthWithin2Pct(s); d.Sign() != 0 {
		t.Fatalf("depth %s with unknown reserves; unknown must not read as deep", d)
	}
}

// The two depth figures answer different questions, and using the wrong one
// took every price source off a running exchange.
//
// RelativeDepth ranks pools against each other on a path that reads no
// balances. DepthWithin2Pct says what can actually be executed and needs them.
// A test rather than a comment because the failure mode was silent: every venue
// reported "no executable depth" and the price loop simply stopped.
func TestRelativeDepthSurvivesWithoutBalances(t *testing.T) {
	p, s := ponsWeth()
	s.Balance0, s.Balance1 = nil, nil

	if d := p.DepthWithin2Pct(s); d.Sign() != 0 {
		t.Errorf("absolute depth without reserves = %s, want 0", d)
	}
	rel := p.RelativeDepth(s)
	if rel.Sign() <= 0 {
		t.Fatal("relative depth is zero without balances; the oracle path reads " +
			"price and liquidity only, so this would drop every venue")
	}
}

// And relative depth must still rank correctly, since ranking is its only job.
func TestRelativeDepthRanksPools(t *testing.T) {
	p, s := ponsWeth()
	thin := *s
	thin.Liquidity = new(big.Int).Quo(s.Liquidity, big.NewInt(10))
	if p.RelativeDepth(&thin).Cmp(p.RelativeDepth(s)) >= 0 {
		t.Fatal("a pool with a tenth of the liquidity did not rank below")
	}
}
