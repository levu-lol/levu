// Package health scores a market and decides what leverage it can safely carry.
//
// This is the piece the whole product rests on: automatic listing is only
// defensible if the thing deciding is harder to fool than the thing it is
// deciding about. Two principles follow from that, and they drive every choice
// in this file.
//
// **Weight signals by what they cost to fake.** On a long-tail token, 24h
// volume is nearly free to manufacture and a spot-TVL snapshot can be added and
// pulled in one block. Token age, holder dispersion and bonded underwriting
// capital cannot. Cheap signals are therefore *gates* — necessary, never
// sufficient — while the score itself is carried by costly ones.
//
// **Price what you can actually hedge or liquidate.** The open-interest cap is
// derived from executable depth, not from market cap, and the maintenance
// margin is derived from what closing the cap would actually cost. A market
// gets the leverage its book can support, not the leverage its headline
// numbers suggest.
//
// Arithmetic is integer basis points throughout: no floats, so a listing
// decision is reproducible on any machine and can be audited after the fact.
package health

import (
	"fmt"
	"math/big"
	"time"

	"github.com/levu-lol/levu/wire"
)

// BPS is the fixed-point scale for every score and ratio here: 10000 == 1.0.
const BPS = 10_000

// Observation is one snapshot of a market. Quantities are in whole quote units
// (read: dollars), which keeps the scoring in int64 and out of Fixed.
type Observation struct {
	Time time.Time

	// --- costly to fake -------------------------------------------------
	// Executable depth within 2% of mid, both sides summed. The single most
	// important input: it is what a liquidation actually eats.
	DepthWithin2Pct int64
	// Bonded first-loss capital backing this market. Cannot be withdrawn
	// instantly, so it is the most expensive signal to counterfeit — and it
	// does double duty: as a costly signal for scoring, and as capital that
	// genuinely extends how much exposure the lane can carry.
	UnderwrittenCapital int64
	// Age of the token.
	Age time.Duration
	// Share of supply held by the largest holder, in basis points.
	TopHolderShareBps int64
	// Share of spot liquidity in the single deepest pool, in basis points.
	TopPoolShareBps int64
	// Oracle confidence from the aggregator, in basis points.
	OracleConfidence int64

	// --- cheap to fake, used only as gates ------------------------------
	Volume24h int64
	SpotTVL   int64
	MarketCap int64

	// BookDepthWithin2Pct is resting depth in *our own* lane, both sides
	// summed, in quote units.
	//
	// Not the same number as DepthWithin2Pct, and the difference is the whole
	// reason this field exists. That one is the spot venue -- Uniswap -- and it
	// says whether the underlying asset can be traded at all. This one is the
	// order book a forced close actually matches against.
	//
	// A new lane has zero here however deep the AMM behind it is. Sizing
	// exposure from venue depth while closing it against this book is how a
	// liquidation becomes a no-op: match_taker finds nothing, closes nothing,
	// and reports success while the position keeps bleeding.
	BookDepthWithin2Pct int64

	// --- market conditions ----------------------------------------------
	// Annualised realised volatility in basis points (10000 == 100%).
	RealisedVolBps int64
	// Current open interest, one side, in quote units.
	OpenInterest int64
}

// Config holds the thresholds and targets. None of these numbers are
// calibrated: they are plausible starting points, and Backtest exists to
// replace them with figures derived from real market history.
type Config struct {
	// Gates. Cheap signals live here and only here.
	MinVolume24h  int64
	MinSpotTVL    int64
	MinDepth      int64
	MinAge        time.Duration
	MinConfidence int64
	// Supply share, in basis points, above which a single holder can end the
	// market unilaterally. This is a gate rather than a score penalty: no
	// amount of depth compensates for one wallet being able to exit into it.
	MaxTopHolderBps int64

	// Targets at which each costly subscore saturates.
	TargetDepth        int64
	TargetUnderwriting int64
	TargetAge          time.Duration
	// Volatility at which the stability subscore reaches zero.
	MaxVolBps int64

	// Subscore weights, in basis points, summing to BPS.
	WeightDepth        int64
	WeightUnderwriting int64
	WeightOracle       int64
	WeightMaturity     int64
	WeightDispersion   int64
	WeightStability    int64

	// Score thresholds for each leverage tier, highest first.
	Tiers []Tier

	// Hysteresis, so a market does not flap between states on noise.
	ListAbove        int64
	ListSustained    time.Duration
	DegradeBelow     int64
	DegradeSustained time.Duration
	// SpotOnlyBelow is where leverage is withdrawn but trading continues at 1x.
	// It sits between DegradeBelow and DelistBelow: a fading market should lose
	// its leverage before it loses its market.
	SpotOnlyBelow     int64
	SpotOnlySustained time.Duration

	DelistBelow     int64
	DelistSustained time.Duration
	// DelistMinDwell is the floor the dwell shrinks to when a reading is bad
	// enough. Without it a catastrophic market waits out the same patient timer
	// as one drifting gently.
	DelistMinDwell time.Duration
	// DelistDepthDropBps is the fall from the window's peak that counts as
	// maximum severity on its own, independent of score.
	DelistDepthDropBps int64
	// DepthWindow is how much history the persistence floors are taken over.
	DepthWindow time.Duration

	// MarkBandBps is how far the mark may sit from the index, in basis points.
	// It is what an attacker has to move the index by to move our mark, so it
	// sets the size of the prize and belongs in the manipulation bound below.
	MarkBandBps int64
	// ManipulationSafety is how many times more expensive moving the index must
	// be than the profit from doing so. One would mean breaking even is enough
	// to deter an attack, which it is not.
	ManipulationSafety int64

	// MaxPositionBps is the largest single position, as a fraction of the OI
	// cap. It is what makes per-position maintenance safe: without it, one
	// account could be the whole lane and closing it would be the systemic
	// event maintenance was sized to avoid.
	MaxPositionBps int64
	// MaxPositionOfBookBps caps a single position as a fraction of the resting
	// depth it would be closed in, so the slippage model is never asked about a
	// position bigger than the book.
	MaxPositionOfBookBps int64
	// MinBookForLeverage is the resting depth below which a market is offered at
	// 1x whatever else it scores. Below it there is a market but not a leveraged
	// one.
	MinBookForLeverage int64
	// InitialBufferX is how many times maintenance the initial margin must be.
	// Two means a position opens at half its liquidation distance.
	InitialBufferX int64
	// MaxLeverage is a hard ceiling, whatever the liquidity says. A safety
	// valve rather than a product decision: the tiers below are where a
	// deployment expresses how much leverage it wants to offer.
	//
	// Setting it above what the arithmetic can reach is not an error but it is
	// a lie: AbsoluteMaxLeverage reports what is actually attainable, and it is
	// bounded by the VM long before it is bounded by this.
	MaxLeverage int64

	// RequireOwnBook makes capacity depend on this lane's own resting depth
	// rather than falling back to the spot venue's.
	//
	// On by default, and the default is the safe one: a lane that cannot read
	// its own book should assume it has none, not assume it has Uniswap's.
	RequireOwnBook bool

	// Fraction of executable depth the protocol is willing to have as open
	// interest, in basis points. The core risk haircut.
	OIFractionOfDepthBps int64
	// How much safe capacity a unit of first-loss capital buys, in basis
	// points. 30000 means each dollar underwritten supports three dollars of
	// exposure beyond what the book alone could absorb — capital covers losses
	// rather than providing liquidity, so it buys more room than the same
	// amount sitting in the book.
	UnderwritingMultipleBps int64
	// Liquidation fee, in basis points of notional.
	LiquidationFeeBps int64
	// Floor on maintenance margin regardless of what the depth model says.
	MinMaintenanceBps int64
}

// Tier maps a score to a maximum leverage.
type Tier struct {
	MinScore int64
	Leverage int64 // whole multiples: 2 means 2x
}

func DefaultConfig() Config {
	return Config{
		MinVolume24h:    50_000,
		MinSpotTVL:      250_000,
		MinDepth:        50_000,
		MinAge:          24 * time.Hour,
		MinConfidence:   5_000,
		MaxTopHolderBps: 5_000,

		// Targets are the level at which a signal stops discriminating, not
		// the level a major reaches. Anchoring them to a blue-chip would make
		// the long tail — the entire wedge — structurally unlistable.
		//
		// UNCALIBRATED: these are judgment, not evidence. Replacing them with
		// figures derived from real Robinhood Chain history is what the
		// backtest harness exists for.
		TargetDepth:        1_000_000,
		TargetUnderwriting: 500_000,
		TargetAge:          7 * 24 * time.Hour,
		MaxVolBps:          40_000, // 400% annualised

		// Spot depth used to carry 3,000 of these 10,000, from when it also
		// bounded capacity. It no longer bounds anything a liquidation touches
		// -- our own book does -- and the one thing it does bound, the cost of
		// faking the index, is now an explicit cap in Derive rather than points
		// here. What is left is a genuine but modest signal: a deep spot market
		// is a real asset being traded by somebody.
		//
		// The weight it gave up went to the two things a perp actually lives
		// with. Volatility decides how often positions have to be liquidated at
		// all, and concentration decides whether one holder can arrange it.
		// Both were underweighted for a product whose dominant loss is
		// liquidating late.
		WeightDepth:        1_500,
		WeightUnderwriting: 2_000,
		WeightOracle:       2_500,
		WeightMaturity:     1_000,
		WeightDispersion:   1_500,
		WeightStability:    1_500,

		// The tier boundaries line up with the state boundaries on purpose, so
		// each state means one thing about leverage:
		//
		//   >= 5500  live        2x / 3x / 5x   (5500 is also the entry)
		//   5000-5499 live        2x            hysteresis: won't newly list
		//   4500-4999 degraded    2x
		//   4000-4499 spot-only   1x
		//   < 4000    reduce-only  -
		//
		// Chosen this way because a state whose name implies a leverage it does
		// not actually grant is worse than having no state at all.
		// These are ceilings on what a *score* has earned, not the leverage
		// itself. The number offered comes out of the liquidity, and these stop
		// a market that happens to be deep but is also young, concentrated or
		// badly priced from being handed everything the book could support.
		//
		// Set generously on purpose: on a market that scores well the binding
		// constraint should be how cheaply it can be closed, which is a fact
		// about the market, rather than a row in this table, which is an
		// opinion about it.
		// Sized to what this configuration can actually deliver, which at a 1%
		// liquidation fee is 33x. Writing 500x here costs nothing and cannot be
		// honoured; TestTheTiersDoNotPromiseMoreThanTheEngineCanDeliver refuses
		// it. Lower the liquidation fee and the ceiling rises -- to 100x at a
		// zero fee, which is where the VM's own liquidation buffer stops it.
		Tiers: []Tier{
			{MinScore: 8_500, Leverage: 33},
			{MinScore: 7_000, Leverage: 20},
			{MinScore: 4_500, Leverage: 10},
			// The spot rung. Without it a market whose score drifts below the
			// lowest leverage tier is offered nothing at all -- not reduced
			// leverage, nothing -- while still having real depth and passing
			// every gate. That cliff pushes every holder to the exit at once.
			{MinScore: 4_000, Leverage: 1},
		},

		// Must equal the lowest tier: any gap between them is a dead zone where
		// a market qualifies for leverage it can never be granted.
		ListAbove:        5_500,
		ListSustained:    30 * time.Minute,
		DegradeBelow:     5_000,
		DegradeSustained: 15 * time.Minute,
		// Below this, leverage comes off entirely and the market trades at 1x.
		// Above DelistBelow on purpose: losing leverage should happen well
		// before losing the market.
		SpotOnlyBelow:     4_500,
		SpotOnlySustained: 20 * time.Minute,
		DelistBelow:       4_000,
		DelistSustained:   2 * time.Hour,
		DelistMinDwell:    5 * time.Minute,
		// A halving of depth inside the window is treated as maximum severity.
		DelistDepthDropBps: 5_000,
		// Long enough that renting liquidity through a listing check costs real
		// money, short enough that a market recovering from a genuine dip is
		// not held down for a day.
		DepthWindow:          2 * time.Hour,
		RequireOwnBook:       true,
		MaxPositionBps:       1_000, // one account may be at most a tenth of the lane
		MaxPositionOfBookBps: 2_500, // and at most a quarter of the book it closes in
		MinBookForLeverage:   250_000,
		InitialBufferX:       2,
		// The VM's own liquidation buffer caps this at 100x whatever is written
		// here; see AbsoluteMaxLeverage.
		MaxLeverage: 100,

		// 50bps, matching the mark band the supervisor pushes to every lane.
		MarkBandBps:        50,
		ManipulationSafety: 4,

		OIFractionOfDepthBps:    5_000,  // OI cap at half of executable depth
		UnderwritingMultipleBps: 30_000, // 3x
		LiquidationFeeBps:       100,    // 1%
		// The floor exists so a market with an implausibly cheap book still
		// carries some maintenance. It is not a leverage decision: at this
		// floor the arithmetic allows 500x, and what actually caps a real
		// market is the liquidation fee plus its own slippage.
		MinMaintenanceBps: 10,
	}
}

// Validate catches a misconfigured weight set before it can list anything.
func (c Config) Validate() error {
	sum := c.WeightDepth + c.WeightUnderwriting + c.WeightOracle +
		c.WeightMaturity + c.WeightDispersion + c.WeightStability
	if sum != BPS {
		return fmt.Errorf("subscore weights sum to %d, want %d", sum, BPS)
	}
	if c.DegradeBelow >= c.ListAbove {
		return fmt.Errorf("degrade threshold %d must sit below the list threshold %d",
			c.DegradeBelow, c.ListAbove)
	}
	if c.DelistBelow >= c.DegradeBelow {
		return fmt.Errorf("delist threshold %d must sit below the degrade threshold %d",
			c.DelistBelow, c.DegradeBelow)
	}
	for i := 1; i < len(c.Tiers); i++ {
		if c.Tiers[i].MinScore >= c.Tiers[i-1].MinScore {
			return fmt.Errorf("tiers must be ordered by descending score")
		}
	}
	if c.DepthWindow <= 0 {
		return fmt.Errorf("health: DepthWindow must be positive; "+
			"a zero window scores the latest reading and makes rented liquidity free again (got %s)", c.DepthWindow)
	}
	if c.DelistMinDwell < 0 || c.DelistMinDwell > c.DelistSustained {
		return fmt.Errorf("health: DelistMinDwell %s must be between zero and DelistSustained %s",
			c.DelistMinDwell, c.DelistSustained)
	}
	if c.DelistDepthDropBps <= 0 {
		return fmt.Errorf("health: DelistDepthDropBps must be positive, got %d", c.DelistDepthDropBps)
	}
	// The window has to be long enough to actually contain the dwell it feeds,
	// or a market can be listed on depth it never held for the listing period.
	if c.DepthWindow < c.ListSustained {
		return fmt.Errorf("health: DepthWindow %s is shorter than ListSustained %s: "+
			"depth could be rented for the listing check and pulled before the window noticed",
			c.DepthWindow, c.ListSustained)
	}
	if c.SpotOnlyBelow >= c.DegradeBelow {
		return fmt.Errorf("health: SpotOnlyBelow %d must sit below DegradeBelow %d",
			c.SpotOnlyBelow, c.DegradeBelow)
	}
	if c.DelistBelow >= c.SpotOnlyBelow {
		return fmt.Errorf("health: DelistBelow %d must sit below SpotOnlyBelow %d: "+
			"a market should lose its leverage before it loses its market",
			c.DelistBelow, c.SpotOnlyBelow)
	}
	if c.SpotOnlySustained <= 0 {
		return fmt.Errorf("health: SpotOnlySustained must be positive")
	}
	if len(c.Tiers) > 0 {
		lowest := c.Tiers[len(c.Tiers)-1].MinScore
		// The spot rung has to be reachable by a market that has fallen into
		// spot-only, or the downgrade lands on a state that can offer nothing —
		// which is the cliff this rung exists to remove.
		if lowest > c.DelistBelow {
			return fmt.Errorf(
				"health: the lowest tier starts at %d but a market is only delisted below %d: "+
					"scores between them are spot-only yet have no tier, so the market would be "+
					"downgraded into offering nothing", lowest, c.DelistBelow)
		}
		if c.Tiers[len(c.Tiers)-1].Leverage != 1 {
			return fmt.Errorf(
				"health: the lowest tier is %dx; it must be 1x, the rung a fading market lands on",
				c.Tiers[len(c.Tiers)-1].Leverage)
		}
		// The entry tier: whatever a newly listed market gets.
		var entry int64
		for _, t := range c.Tiers {
			if c.ListAbove >= t.MinScore {
				entry = t.Leverage
				break
			}
		}
		if entry == 0 {
			return fmt.Errorf(
				"health: a market listing at %d matches no tier (lowest is %d), so it would "+
					"be listed and immediately unable to offer anything", c.ListAbove, lowest)
		}
	}
	return nil
}

// Subscores breaks the score into its parts, so a decision can be explained
// rather than merely announced.
type Subscores struct {
	Depth        int64
	Underwriting int64
	Oracle       int64
	Maturity     int64
	Dispersion   int64
	Stability    int64
}

// Score is a single market assessment.
type Score struct {
	Total int64
	Sub   Subscores
	// GateFailures lists the gates that did not pass. A non-empty list means no
	// market at any score: there is no asset here worth listing.
	GateFailures []string
	// LeverageBlockers lists conditions under which the market is real and
	// tradeable but must not be levered.
	//
	// Separate from GateFailures because they answer different questions, and
	// running them together was a mistake worth naming. "Is there an asset
	// here" is about volume, backing, age and who holds it. "Can we lend
	// against it" is about how cheaply the price we mark against can be moved.
	// A market can pass the first and fail the second, and the honest response
	// to that is the spot rung, not a refusal.
	LeverageBlockers []string
}

// Eligible reports whether every gate passed: whether there is a market here.
func (s Score) Eligible() bool { return len(s.GateFailures) == 0 }

// Leverable reports whether this market may be lent against.
//
// A market can be perfectly tradeable and not levered. At 1x a position is
// fully collateralised, so forcing a liquidation means moving the mark by
// almost its whole value -- which against a thin pool means draining it, not
// nudging it. At 10x the same attack needs a ten percent move. The danger
// scales with the leverage, so that is what comes off.
func (s Score) Leverable() bool { return len(s.LeverageBlockers) == 0 }

// ratio returns v/target in basis points, capped at BPS and floored at zero.
//
// The multiply is done in big.Int rather than int64. That is not caution for
// its own sake: `Age` is a time.Duration in *nanoseconds*, so a 200-day-old
// token is 1.7e16, and 1.7e16 * 10000 overflows int64 and wraps to garbage —
// silently scoring a mature token as immature. Scoring runs once per
// observation, not per order, so the allocation is irrelevant next to being
// right.
func ratio(v, target int64) int64 {
	if target <= 0 || v <= 0 {
		return 0
	}
	if v >= target {
		return BPS
	}
	n := new(big.Int).Mul(big.NewInt(v), big.NewInt(BPS))
	n.Quo(n, big.NewInt(target))
	if !n.IsInt64() {
		return BPS
	}
	r := n.Int64()
	if r > BPS {
		return BPS
	}
	return r
}

// Assess scores one observation.
func Assess(o Observation, cfg Config) Score {
	var s Score

	// Gates: cheap signals are necessary, never sufficient. Failing one means
	// no market regardless of how good everything else looks.
	if o.Volume24h < cfg.MinVolume24h {
		s.GateFailures = append(s.GateFailures, "24h volume below floor")
	}
	if o.SpotTVL < cfg.MinSpotTVL {
		s.GateFailures = append(s.GateFailures, "spot TVL below floor")
	}
	if o.Age < cfg.MinAge {
		s.GateFailures = append(s.GateFailures, "token too young")
	}

	// Not gates. These say the venue we price against is thin or singular, so
	// the mark is cheap to move -- which bounds leverage and says nothing about
	// whether the market should exist.
	//
	// Venue depth in particular was a gate until it was noticed that it gates
	// on the *chain's* liquidity while capacity comes from our own book. A
	// market with $15k on Uniswap and a real book of our own was being refused
	// for the thinness of somebody else's pool.
	if o.DepthWithin2Pct < cfg.MinDepth {
		s.LeverageBlockers = append(s.LeverageBlockers,
			"the venue we price against is too thin to lend against")
	}
	if o.OracleConfidence < cfg.MinConfidence {
		s.LeverageBlockers = append(s.LeverageBlockers,
			"one thin price source: a bad print cannot be told from a real one")
	}
	if cfg.MaxTopHolderBps > 0 && o.TopHolderShareBps > cfg.MaxTopHolderBps {
		s.GateFailures = append(s.GateFailures, "supply too concentrated in one holder")
	}

	// Costly subscores carry the score itself.
	//
	// Depth takes the better of the two books, because they are evidence of the
	// same thing at different stages of a lane's life. Before makers arrive our
	// book is empty and the venue is all we know; once they have arrived our own
	// book is the better witness, and it is the one that sizes positions. Taking
	// the venue alone was a third instance of the mistake the gates had: reading
	// somebody else's pool for a number about the market we run. Max rather than
	// a swap keeps it monotone -- quoting on our own book can only ever raise
	// this, never lower it -- and it opens no hole, because the manipulation cap
	// still bounds open interest by the venue depth regardless of what we score
	// here.
	ourDepth := o.BookDepthWithin2Pct
	if o.DepthWithin2Pct > ourDepth {
		ourDepth = o.DepthWithin2Pct
	}
	s.Sub.Depth = ratio(ourDepth, cfg.TargetDepth)
	s.Sub.Underwriting = ratio(o.UnderwrittenCapital, cfg.TargetUnderwriting)
	s.Sub.Oracle = clamp(o.OracleConfidence, 0, BPS)
	s.Sub.Maturity = ratio(int64(o.Age), int64(cfg.TargetAge))

	// Dispersion: concentrated supply or single-pool liquidity is a market one
	// participant can move alone.
	//
	// Zero means unknown, not perfect, and is scored as the worst case.
	//
	// The natural reading of a zero here is "no concentration", which awards
	// full marks to an operator who supplied no data at all -- scoring a market
	// *better* than one whose figures were measured. Neither of these is
	// plausibly zero in a live market: there is always a largest holder and
	// always a deepest pool. So zero is missing data, and missing data about
	// concentration is exactly the case that should not be given the benefit of
	// the doubt.
	holder, pool := o.TopHolderShareBps, o.TopPoolShareBps
	if holder <= 0 {
		holder = BPS
	}
	if pool <= 0 {
		pool = BPS
	}
	worst := max64(holder, pool)
	s.Sub.Dispersion = clamp(BPS-worst, 0, BPS)

	// Stability falls linearly to zero at MaxVolBps. Zero volatility is the
	// same kind of claim as zero concentration: a price that has never moved is
	// a price nobody is trading, so it reads as unknown and scores as the worst
	// case rather than as perfect calm.
	vol := o.RealisedVolBps
	if vol <= 0 {
		vol = cfg.MaxVolBps
	}
	if cfg.MaxVolBps > 0 {
		s.Sub.Stability = clamp(BPS-ratio(vol, cfg.MaxVolBps), 0, BPS)
	} else {
		s.Sub.Stability = BPS
	}

	s.Total = (s.Sub.Depth*cfg.WeightDepth +
		s.Sub.Underwriting*cfg.WeightUnderwriting +
		s.Sub.Oracle*cfg.WeightOracle +
		s.Sub.Maturity*cfg.WeightMaturity +
		s.Sub.Dispersion*cfg.WeightDispersion +
		s.Sub.Stability*cfg.WeightStability) / BPS

	return s
}

// Capacity is the risk envelope a scored market can carry.
type Capacity struct {
	// Openable is false when the market may not take on new exposure. A
	// Capacity with Openable false retains the *previous* margin requirements
	// so open positions stay measured consistently — it must never be a zero
	// value, because a zero initial margin reads as infinite leverage.
	Openable    bool
	MaxLeverage int64
	// OI cap per side, in quote units.
	OICap int64
	// Maintenance margin in basis points of notional.
	MaintenanceBps int64
	// Initial margin in basis points, derived from the leverage tier.
	InitialBps int64
	// MarginSlopePerUnit is the extra margin fraction charged per unit of
	// notional, so a trader's actual leverage falls as their position grows.
	// Derived from the book, in WAD.
	MarginSlopePerUnit int64
	// MaxPosition is the largest single position the lane will carry, in quote
	// units. Bounded both as a share of the lane and as a share of the book it
	// would be closed in, so it is also the number maintenance was sized
	// against rather than a separate policy.
	MaxPosition int64
	// Liquidation fee the maintenance figure was sized against. Carried on the
	// capacity so the emitted params are self-consistent: sizing maintenance
	// for one fee and emitting another would break the VM's own invariant that
	// maintenance must cover the cost of liquidating.
	LiquidationFeeBps int64
}

// vmLiquidationBufferBps mirrors RiskParams::MIN_LIQUIDATION_BUFFER in
// perpvm/src/types.rs. Duplicated across the boundary and held there by
// TestEveryEmittedParameterSetIsAcceptedByTheVM, which pushes what this engine
// emits into a real VM and fails if it is refused.
const vmLiquidationBufferBps int64 = 50

// wadPerUnit is 1e18: the fixed-point scale the VM works in.
const wadPerUnit int64 = 1_000_000_000_000_000_000

// LeverageAt is the leverage a position of this notional actually receives,
// which is what a trader should be shown before they commit to a size.
//
// The market's headline number is what the largest permitted position gets. A
// trader asking for more than the book can carry does not get a warning and a
// fill at the number they wanted; they get the number the book can carry.
// rateAt is the initial margin a position of this size must post, in basis
// points of notional.
//
// This is the primitive; leverage and margin are both views of it.
func (c Capacity) rateAt(notional int64) int64 {
	if c.InitialBps <= 0 {
		return 0
	}
	rate := c.InitialBps
	if c.MarginSlopePerUnit > 0 && notional > 0 {
		// Through big.Int, because the intermediate does not fit.
		//
		// The slope is a WAD fraction and the rate is in basis points, and the
		// first version of this mixed the two: notional x slope x 2 / WAD left
		// as int64 divides 1e16 by 1e18 and truncates to zero, so the term
		// silently vanished and every size reported the headline leverage. A
		// unit error that reads as "no effect" is worse than one that reads as
		// nonsense, because nothing about the output looks wrong.
		//
		// The buffer that keeps initial clear of maintenance applies to the
		// size-dependent part too, matching RiskParams::initial_rate_for.
		extra := new(big.Int).Mul(big.NewInt(notional), big.NewInt(c.MarginSlopePerUnit))
		extra.Mul(extra, big.NewInt(2*BPS))
		extra.Quo(extra, big.NewInt(wadPerUnit))
		if !extra.IsInt64() {
			return 0 // a position this size is not offerable at any rate
		}
		rate += extra.Int64()
	}
	return rate
}

// MarginAt is what a position of this size must post, in quote units.
//
// The honest quantity, and the one a trader can act on. Leverage is a rounded
// view of it and stops being informative exactly where it matters most: on a
// fully collateralised market the slope pushes the rate above 100%, which is
// correct and safe, and which integer leverage can only render as zero.
func (c Capacity) MarginAt(notional int64) int64 {
	if c.InitialBps <= 0 || notional <= 0 {
		return 0
	}
	// In the VM's arithmetic, not an approximation of it.
	//
	// rateAt works in integer basis points, and the slope term at a typical
	// paper size is a fraction of one: 105 notional at a slope of 1.67e-7 per
	// unit adds 0.00035bps, which rateAt truncates to nothing. The VM applies
	// the same slope in fixed point and wanted 105.0037 against the 105 this
	// used to answer -- and refused the order. Mirror RiskParams::initial_rate_for
	// exactly: rate = base + notional*slope*2, margin = notional*rate, then
	// round up to whole units, so what the lane is funded is never less than
	// what the VM will ask.
	n := wire.FixedWhole(notional)
	rate := bpsToFixed(c.InitialBps)
	if c.MarginSlopePerUnit > 0 {
		rate = rate.Add(n.Mul(wire.FixedRawInt64(c.MarginSlopePerUnit)).Mul(wire.FixedWhole(2)))
	}
	m := n.Mul(rate)
	whole := m.Whole()
	if wire.FixedWhole(whole).Cmp(m) < 0 {
		whole++
	}
	return whole
}

// LeverageAt is the leverage a position of this size is offered.
//
// Zero means the size is not offerable at all. It does not mean "less than
// one": a market on the spot rung posts full collateral plus whatever the
// slope adds, which is a rate above 100% and a perfectly good trade. Integer
// division reported that as zero, so every order on every spot market was
// refused as unofferable -- which was every market on the chain.
func (c Capacity) LeverageAt(notional int64) int64 {
	rate := c.rateAt(notional)
	if rate <= 0 {
		return 0
	}
	if lev := BPS / rate; lev >= 1 {
		return lev
	}
	// Payable, just not at a whole multiple. The extra margin is visible
	// through MarginAt, which is where it belongs.
	return 1
}

// AbsoluteMaxLeverage is the most leverage this configuration can ever produce,
// whatever the market.
//
// Not a policy number. The VM refuses any lane whose maintenance does not exceed
// its liquidation fee by 50bps -- liquidating has to leave the account solvent
// on the fee alone -- so maintenance has a hard floor even at a zero fee, and
// initial margin has to clear maintenance by the buffer. Those two together set
// a ceiling nothing can climb past:
//
//	100x at a zero liquidation fee, 33x at the 1% default.
//
// Worth computing rather than assuming. A tier table saying 500x costs nothing
// to write and cannot be delivered, and the place that mistake surfaces is a
// marketing page rather than a test.
func AbsoluteMaxLeverage(cfg Config) int64 {
	buffer := cfg.InitialBufferX
	if buffer < 2 {
		buffer = 2
	}
	floor := cfg.MinMaintenanceBps
	if f := cfg.LiquidationFeeBps + vmLiquidationBufferBps; f > floor {
		floor = f
	}
	if b := cfg.MarkBandBps + 1; b > floor {
		floor = b
	}
	if floor <= 0 {
		return 0
	}
	lev := BPS / (buffer * floor)
	if cfg.MaxLeverage > 0 && lev > cfg.MaxLeverage {
		lev = cfg.MaxLeverage
	}
	return lev
}

// manipulationCap is the most exposure a lane may carry before moving the index
// becomes profitable.
//
// Venue depth is quoted within 2% of mid. The band is usually far narrower, and
// over a small move the quote consumed scales roughly with the move, so depth
// inside the band is scaled down from the 2% figure rather than measured
// separately. Roughly, and deliberately so: the error is a fraction, while the
// thing it guards against is a market whose price costs a few thousand dollars
// to move.
func manipulationCap(venueDepth2Pct int64, cfg Config) int64 {
	if venueDepth2Pct <= 0 || cfg.MarkBandBps <= 0 {
		return 0
	}
	safety := cfg.ManipulationSafety
	if safety < 1 {
		safety = 1
	}
	// depth inside the band, from depth inside 2% (= 200bps).
	depthInBand := venueDepth2Pct * cfg.MarkBandBps / 200
	if depthInBand <= 0 {
		return 0
	}
	// profit = P * band, so P = depth / band. Both in bps, so multiply by BPS.
	return depthInBand * BPS / cfg.MarkBandBps / safety
}

// Derive turns a score and an observation into a risk envelope.
//
// The OI cap comes from executable depth rather than the score, because the
// question a cap answers is not "how good is this market" but "how much
// exposure can we actually close". The maintenance margin then covers what
// closing that cap would cost — the liquidation fee plus the slippage of eating
// the book — which is what keeps liquidation from being loss-making by
// construction.
func Derive(o Observation, s Score, cfg Config) (Capacity, bool) {
	if !s.Eligible() {
		return Capacity{}, false
	}
	// The tiers say how much leverage a score has *earned*. They do not decide
	// whether there is a market, because the gates already did that.
	//
	// A token that clears every gate is a real asset with real volume and real
	// backing. Offering it at 1x, fully collateralised, risks nothing we have
	// to underwrite: the trader's own margin covers their whole position. So a
	// score below the lowest tier withdraws leverage and leaves the spot rung,
	// which is the bottom of the ladder rather than the exit from it.
	//
	// Refusing here instead put our own balance sheet in the way of listing:
	// the underwriting subscore carries 2000 of 10000, so 51 markets that clear
	// every gate were refused for capital *we* had not posted, against assets
	// whose quality had not changed. Whether a market stays listed as it decays
	// is the lifecycle's DelistBelow, and that is where it belongs.
	lev := int64(1)
	for _, t := range cfg.Tiers {
		if s.Total >= t.MinScore {
			lev = t.Leverage
			break
		}
	}

	// Safe capacity is what the book can absorb plus what first-loss capital
	// can cover. This is the mechanism that lets demand for leverage finance
	// its own safety: underwriters are paid capacity rent, and the capacity
	// they create is real rather than an accounting fiction.
	// Closable capacity, not tradeable capacity.
	//
	// The venue's depth says the underlying can be traded; it does not say this
	// lane can close a position, because a forced close matches in our own book
	// and not on Uniswap. So the book bounds what liquidation can eat, and
	// underwriting capital extends it -- capital absorbs a loss the book could
	// not clear, which is exactly what first-loss capital is for.
	//
	// A lane whose book is empty is therefore capped at whatever underwriting
	// backs it, and nothing else. That is the correct answer to "we have deep
	// spot liquidity but no book yet": the market is real, our side of it is
	// not, and only the second one can absorb a liquidation.
	closable := o.BookDepthWithin2Pct
	if cfg.RequireOwnBook && closable <= 0 {
		closable = 0
	} else if !cfg.RequireOwnBook {
		// Legacy behaviour, retained for scenario fixtures and any caller that
		// genuinely has no book reading: fall back to venue depth.
		if closable <= 0 {
			closable = o.DepthWithin2Pct
		}
	}
	// Whether this lane lends at all is decided before capacity is sized,
	// because listing and lending are bounded by different things and sizing
	// them the same way is how a market gets refused for the wrong reason.
	//
	// Everything below about closable capacity is about *leverage*: what a
	// forced close can eat, and what first-loss capital can absorb when the
	// book cannot clear it. A fully collateralised position has neither
	// problem. The trader's own margin covers the entire notional, so the lane
	// is never underwater, and there is no loss for our capital to absorb.
	//
	// So an empty book does not make a spot market unlistable. It makes orders
	// hard to fill, which is a matter for the matching engine at the moment a
	// trade is attempted, not a reason to refuse the listing. Conflating the
	// two refused 57 perfectly good markets for liquidity we had not supplied
	// yet -- and would never supply, because a market nobody can list is a
	// market no maker quotes.
	spot := !s.Leverable() ||
		(cfg.MinBookForLeverage > 0 && closable < cfg.MinBookForLeverage)

	depthCapacity := closable * cfg.OIFractionOfDepthBps / BPS
	underwritingCapacity := o.UnderwrittenCapital * cfg.UnderwritingMultipleBps / BPS
	oiCap := depthCapacity + underwritingCapacity
	if spot {
		lev = 1
		// The one bound that still applies to a fully collateralised lane is
		// the cost of faking the mark: a wrong mark still settles PnL and still
		// triggers liquidations, whatever the leverage. If we cannot even bound
		// that -- no measurable venue depth -- there is nothing to list against.
		oiCap = manipulationCap(o.DepthWithin2Pct, cfg)
	}

	// What spot depth is actually for.
	//
	// The index is aggregated from the spot venues, so moving those venues is
	// how our mark gets faked. An attacker holding P profits P x band from
	// dragging the mark across the band, and it costs roughly the venue depth
	// inside that band to drag it. So manipulation pays whenever
	//
	//     P x band > depth_in_band
	//
	// and the cap that prevents it is depth_in_band / band, divided again by a
	// safety multiple because breaking even is not a deterrent.
	//
	// This is the term that used to be missing. Spot depth carried a third of
	// the health score as a general measure of goodness, which is not a thing
	// it measures: after the book fix it bounds nothing a liquidation touches.
	// What it bounds is the cost of lying about the price, and that belongs
	// here, as a cap on exposure, rather than as points on a score.
	if cap := manipulationCap(o.DepthWithin2Pct, cfg); cap > 0 && cap < oiCap {
		oiCap = cap
	}

	// Slippage of closing the cap against the book. Depth is measured within
	// 2%, so consuming a fraction f of it costs roughly f*2% on average — a
	// linear-book approximation, deliberately crude and deliberately
	// conservative, since a real book thins as you eat it.
	// Maintenance covers closing *one* liquidated position, not the whole lane.
	//
	// Sizing it against the entire OI cap was the old behaviour and it is the
	// reason leverage could never go high however deep a market was: it assumes
	// every account is liquidated in the same instant, which prices a systemic
	// event into every individual position. A liquidation closes one account,
	// so the cost that maintenance has to cover is the cost of closing the
	// largest single position the lane permits.
	//
	// Concentration risk does not disappear, it moves to where it belongs --
	// MaxPositionBps caps how much of the lane one account may be, and the OI
	// cap still bounds the whole.
	maxPosition := oiCap * cfg.MaxPositionBps / BPS
	if maxPosition <= 0 {
		maxPosition = oiCap
	}
	// A position may not exceed a fraction of the book it has to be closed in.
	//
	// Without this the OI cap is inflated by underwriting -- which absorbs loss
	// but does not fill orders -- and the linear slippage model then quotes a
	// finite cost for closing a position larger than the entire book. Measured
	// before this bound: a $25k book offered 7x on positions of $76k, three
	// times the whole book, at a maintenance margin of 7.1%. That trade cannot
	// be closed at any price, and the arithmetic said it could.
	//
	// The bound also makes the model honest rather than merely conservative.
	// Constant-liquidity slippage is a reasonable approximation while a position
	// is a small part of the book and nonsense once it is not, so the position
	// is held inside the range where the approximation holds.
	// This bound applies to a spot lane too, and excluding it was a mistake.
	//
	// The reasoning for excluding it was that a fully collateralised position
	// has no loss for capital to absorb, which is true and is about solvency.
	// Closing is a different question: a position larger than the book cannot
	// be got out of at any price, and being fully collateralised does not help
	// with that. The effect was inverted -- a market with a $50,000 book
	// permitted a $2.5m position while one with a $250,000 book permitted
	// $12,500, because only the second was reaching this line.
	//
	// A book of exactly zero is left to the OI cap rather than pinned to zero.
	// Not indulgence: a position can only be created by a fill, a fill needs a
	// counterparty, and a counterparty is depth -- so the cap here is
	// unreachable in that state, while pinning it to zero would refuse the
	// resting orders that are the only way a book ever starts.
	if byBook := closable * cfg.MaxPositionOfBookBps / BPS; byBook > 0 && byBook < maxPosition {
		maxPosition = byBook
	}

	// An envelope that permits no position is not a market.
	//
	// Every bound above is a ceiling, and nothing until here asked whether they
	// left any room. With no underwriting and no book of our own the OI cap is
	// zero, and the rest of this function then computed a perfectly coherent
	// risk envelope around it: a maintenance rate, a leverage figure, a slope,
	// and ok=true. A lane that reports itself live at 1x while permitting zero
	// open interest is worse than a refusal, because the refusal is the honest
	// answer and the lane is a market nobody can trade in.
	//
	// It also produced an inversion that made the gap obvious: underwriting a
	// lane with $500k and no book *refused* it -- correctly, closing is
	// impossible at any price -- while underwriting it with nothing let it
	// through. More capital cannot mean fewer markets.
	if oiCap <= 0 || maxPosition <= 0 {
		return Capacity{}, false
	}
	slippageBps := int64(0)
	switch {
	case spot:
		// Nothing to size: the position is covered by its own collateral, so
		// maintenance falls to the floor the VM requires and no further.
	case closable > 0:
		slippageBps = maxPosition * 200 / closable
	case oiCap > 0:
		// Leveraged exposure backed only by capital, with no book to close
		// into. Closing is not possible at any price, so maintenance lands at
		// the ceiling and the market is refused.
		slippageBps = BPS
	}
	maintenance := cfg.LiquidationFeeBps + slippageBps
	// The floor is derived, not just configured, because the VM enforces two
	// relationships and will refuse parameters that break them:
	//
	//   maintenance >= liquidation fee + 50bps   liquidating must leave the
	//                                            account solvent on the fee
	//   maintenance >  mark band                 or moving the book alone could
	//                                            push accounts into liquidation
	//
	// Emitting anything else produces a lane that rejects its own parameters at
	// the moment they are pushed, which is a configuration error discovered at
	// the worst time. Taking the maximum here means the two sides agree by
	// construction rather than by both being set carefully.
	//
	// It is also what really caps leverage. Wanting 500x is wanting a
	// liquidation fee and a mark band small enough to allow it, which a deep
	// market can carry and a thin one cannot.
	floor := cfg.MinMaintenanceBps
	if f := cfg.LiquidationFeeBps + vmLiquidationBufferBps; f > floor {
		floor = f
	}
	if b := cfg.MarkBandBps + 1; b > floor {
		floor = b
	}
	if maintenance < floor {
		maintenance = floor
	}

	// Below a floor of real resting depth, no leverage at any score.
	//
	// The curve degrades smoothly and that is not enough on its own: smooth
	// degradation still hands 12x to a market whose whole book is $50k, because
	// nothing in the arithmetic says a book can be too small to lever against at
	// all. This says it. Under the floor the market is still perfectly tradeable
	// -- it drops to spot, it does not close -- which is the rung that exists
	// for exactly this.
	if cfg.MinBookForLeverage > 0 && closable < cfg.MinBookForLeverage {
		lev = 1
	}
	// And the same for a price we cannot lend against, for the same reason: the
	// market is real, our confidence in its mark is not.
	if !s.Leverable() {
		lev = 1
	}

	// Leverage comes out of the liquidity, not out of a table.
	//
	// Initial margin has to clear maintenance by a margin of its own, or a
	// position opens already close to liquidation and the first tick against it
	// is a liquidation. With that buffer fixed, the most leverage a market can
	// carry is a pure consequence of how cheaply it can be closed:
	//
	//     maxLeverage = 10000 / (buffer x maintenance)
	//
	// A market deep enough for single-digit-bps maintenance supports leverage in
	// the hundreds, and one that costs 3% to close supports about fifteen. There
	// is no tier table in that sentence, which is the point: the ladder in the
	// config is a *cap* on what a score has earned, never the source of the
	// number.
	buffer := cfg.InitialBufferX
	if buffer < 2 {
		buffer = 2
	}
	byLiquidity := BPS / (buffer * maintenance)
	if byLiquidity < lev {
		lev = byLiquidity
	}
	if cfg.MaxLeverage > 0 && lev > cfg.MaxLeverage {
		lev = cfg.MaxLeverage
	}
	if lev < 1 {
		return Capacity{}, false
	}

	initial := BPS / lev
	// Belt and braces: the arithmetic above should already guarantee this, and
	// a rounding case that slipped through would open positions liquidatable.
	for initial <= maintenance && lev > 1 {
		lev--
		initial = BPS / lev
	}
	if initial <= maintenance {
		return Capacity{}, false
	}

	// The slope a trader's own size is charged against.
	//
	// The market-wide leverage figure is what the *largest permitted* position
	// gets. Everyone smaller than that is being overcharged by a flat rate, and
	// everyone who wants more than the book can carry is being undercharged
	// right up to the moment it matters. The slope replaces one number with the
	// line it was a point on: closing N of a book B consumes about N/B of the
	// 2% band, so the extra margin per unit of notional is 0.02/B.
	slope := int64(0)
	if closable > 0 {
		slope = wadPerUnit * 2 / 100 / closable
	}

	return Capacity{
		Openable:           true,
		MaxLeverage:        lev,
		OICap:              oiCap,
		MaxPosition:        maxPosition,
		MarginSlopePerUnit: slope,
		MaintenanceBps:     maintenance,
		InitialBps:         initial,
		LiquidationFeeBps:  cfg.LiquidationFeeBps,
	}, true
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// bpsToFixed converts basis points to a WAD fraction.
func bpsToFixed(bps int64) wire.Fixed {
	return wire.FixedWhole(bps).Div(wire.FixedWhole(BPS))
}

// RiskParams renders a capacity as the struct the VM enforces.
//
// This is the whole point of the pluggable interface: the engine emits exactly
// what a static stub emits, so swapping it in touches no execution code.
func (c Capacity) RiskParams(base wire.RiskParams, markBand wire.Fixed) (wire.RiskParams, error) {
	if c.InitialBps <= 0 || c.MaintenanceBps <= 0 {
		return base, fmt.Errorf(
			"refusing to emit params from an uninitialised capacity: a zero initial " +
				"margin would read as infinite leverage")
	}
	if c.InitialBps <= c.MaintenanceBps {
		return base, fmt.Errorf("initial margin %d must exceed maintenance %d",
			c.InitialBps, c.MaintenanceBps)
	}
	p := base
	p.LiquidationFee = bpsToFixed(c.LiquidationFeeBps)
	p.InitialMarginLong = bpsToFixed(c.InitialBps)
	p.InitialMarginShort = bpsToFixed(c.InitialBps)
	p.MaintenanceMargin = bpsToFixed(c.MaintenanceBps)
	p.OICapLong = wire.FixedWhole(c.OICap)
	p.OICapShort = wire.FixedWhole(c.OICap)
	p.MarkBand = markBand
	p.MarginSlope = wire.FixedRawInt64(c.MarginSlopePerUnit)
	// The engine never force-closes. When a market may not open, it goes
	// reduce-only and positions wind down voluntarily.
	p.ReduceOnly = !c.Openable
	return p, nil
}
