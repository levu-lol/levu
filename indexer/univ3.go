package indexer

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/levu-lol/levu/health"
)

// V3Pool is a concentrated-liquidity pool.
type V3Pool struct {
	Symbol string
	// Address of the pool.
	Pool string
	// Token addresses, needed for balances and decimals.
	Token0, Token1 string
	// Decimals for each side.
	Decimals0, Decimals1 uint8
	// QuoteIsToken0 orients price and depth.
	QuoteIsToken0 bool
	// BaseSupply is the base token's total supply, for market cap. Optional.
	BaseSupply *big.Int
	// DepthHaircutBps discounts the constant-liquidity depth estimate.
	//
	// The estimate below assumes liquidity is flat across the ±2% band, which
	// holds only while no tick boundary is crossed. Real concentrated liquidity
	// usually thins outside the active range, so the raw figure is an
	// *over*estimate — and over-estimating depth is the dangerous direction,
	// because it grants leverage the book cannot actually absorb. The haircut
	// is what keeps the error pointing the safe way.
	DepthHaircutBps int64
}

// Two constants in 1e18 fixed point.
var (
	// sqrt(1.02) - 1
	sqrt102Minus1 = big.NewInt(9_950_493_836_207_795)
	// 1 - 1/sqrt(1.02), 1e18-scaled. The token0 counterpart of the above: a 2%
	// move consumes L*sqrtP*(sqrt(1.02)-1) of token1, but L/sqrtP*(1-1/sqrt(1.02))
	// of token0.
	oneMinusInvSqrt102 = big.NewInt(9_852_457_023_325_691)
	two96              = new(big.Int).Lsh(big.NewInt(1), 96)
	oneE18             = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

// V3State is what a pool reports about itself right now.
type V3State struct {
	SqrtPriceX96 *big.Int
	Liquidity    *big.Int
	Balance0     *big.Int
	Balance1     *big.Int
}

// ReadV3Price fetches only what pricing needs: the square-root price and the
// active liquidity.
//
// Token balances are needed for TVL and for nothing else, and fetching them
// doubles the number of round trips. On a rate-limited endpoint that is the
// difference between a venue answering inside its timeout and being dropped —
// so the oracle path reads two values and the health path reads four.
func (o *Observer) ReadV3Price(ctx context.Context, p V3Pool) (*V3State, error) {
	slot0, err := o.chain.Call(ctx, p.Pool, "slot0()(uint160,int24,uint16,uint16,uint16,uint8,bool)")
	if err != nil || len(slot0) == 0 {
		return nil, fmt.Errorf("slot0: %w", err)
	}
	sqrtP, ok := new(big.Int).SetString(slot0[0], 10)
	if !ok {
		return nil, fmt.Errorf("parse sqrtPriceX96 %q", slot0[0])
	}
	liq, err := o.chain.Call(ctx, p.Pool, "liquidity()(uint128)")
	if err != nil || len(liq) == 0 {
		return nil, fmt.Errorf("liquidity: %w", err)
	}
	l, ok := new(big.Int).SetString(liq[0], 10)
	if !ok {
		return nil, fmt.Errorf("parse liquidity %q", liq[0])
	}
	return &V3State{
		SqrtPriceX96: sqrtP, Liquidity: l,
		Balance0: big.NewInt(0), Balance1: big.NewInt(0),
	}, nil
}

// ReadV3 fetches live pool state, including balances for TVL.
func (o *Observer) ReadV3(ctx context.Context, p V3Pool) (*V3State, error) {
	slot0, err := o.chain.Call(ctx, p.Pool, "slot0()(uint160,int24,uint16,uint16,uint16,uint8,bool)")
	if err != nil || len(slot0) == 0 {
		return nil, fmt.Errorf("slot0: %w", err)
	}
	sqrtP, ok := new(big.Int).SetString(slot0[0], 10)
	if !ok {
		return nil, fmt.Errorf("parse sqrtPriceX96 %q", slot0[0])
	}

	liq, err := o.chain.Call(ctx, p.Pool, "liquidity()(uint128)")
	if err != nil || len(liq) == 0 {
		return nil, fmt.Errorf("liquidity: %w", err)
	}
	l, ok := new(big.Int).SetString(liq[0], 10)
	if !ok {
		return nil, fmt.Errorf("parse liquidity %q", liq[0])
	}

	bal := func(token string) (*big.Int, error) {
		out, err := o.chain.Call(ctx, token, "balanceOf(address)(uint256)", p.Pool)
		if err != nil || len(out) == 0 {
			return nil, fmt.Errorf("balanceOf: %w", err)
		}
		v, ok := new(big.Int).SetString(out[0], 10)
		if !ok {
			return nil, fmt.Errorf("parse balance %q", out[0])
		}
		return v, nil
	}
	b0, err := bal(p.Token0)
	if err != nil {
		return nil, err
	}
	b1, err := bal(p.Token1)
	if err != nil {
		return nil, err
	}
	return &V3State{SqrtPriceX96: sqrtP, Liquidity: l, Balance0: b0, Balance1: b1}, nil
}

// DepthWithin2Pct is the quote-denominated liquidity executable within ±2%.
//
// For a concentrated-liquidity pool at constant L, moving the price up by d
// consumes `L * sqrtP * (sqrt(1+d) - 1) / 2^96` of the quote token. Moving it
// down by the same factor consumes an amount of the base token worth exactly
// the same in quote terms, so both sides together are twice that figure.
//
// Returned in whole quote units, after the haircut.
// DepthWithin2Pct is the quote-denominated liquidity executable within 2% of
// mid, both sides summed.
//
// The constant-liquidity estimate assumes L holds flat across the band, and on
// a concentrated position that is not merely optimistic, it is impossible. On
// Robinhood Chain's PONS/WETH pools it reported 156x and 320x the pool's entire
// token balance -- depth that could not exist, because the pool does not hold
// the tokens to pay it out.
//
// So each side is capped by the reserve that side would have to be paid from.
// Moving the price up means the pool sells base and it can only sell what it
// has; moving it down means paying out quote, likewise. This is a hard physical
// bound, not a further haircut, and it binds exactly where the flat-liquidity
// assumption fails worst.
// sideWithin2Pct is one side of a 2% move, in raw quote units.
//
// Which closed form applies depends on which token the quote is: a 2% move
// consumes L*sqrtP*(sqrt(1.02)-1) of token1, but L/sqrtP*(1-1/sqrt(1.02)) of
// token0. Using the token1 form for a token0 quote overstates by a factor of
// the raw price -- 2.6e11x measured on a 6-decimal quote against an 18-decimal
// base. Both callers below need it, and both had it wrong the same way.
func (p V3Pool) sideWithin2Pct(s *V3State) *big.Int {
	if s.SqrtPriceX96 == nil || s.SqrtPriceX96.Sign() == 0 || s.Liquidity == nil {
		return big.NewInt(0)
	}
	side := new(big.Int)
	if p.QuoteIsToken0 {
		side.Mul(s.Liquidity, two96)
		side.Mul(side, oneMinusInvSqrt102)
		side.Quo(side, s.SqrtPriceX96)
	} else {
		side.Mul(s.Liquidity, s.SqrtPriceX96)
		side.Mul(side, sqrt102Minus1)
		side.Quo(side, two96)
	}
	return side.Quo(side, oneE18)
}

func (p V3Pool) DepthWithin2Pct(s *V3State) *big.Int {
	// One side of a 2% move, in raw *quote* units -- which of the two closed
	// forms applies depends on which token the quote is.
	//
	// Getting this wrong is not a rounding error. Using the token1 form when the
	// quote is token0 overstates the estimate by a factor of the raw price:
	// measured at 2.6e11x on a 6-decimal quote against an 18-decimal base. The
	// reserve bounds below hide it -- depth saturates to roughly TVL instead of
	// running away -- which is exactly why it survived. USDG sorts low enough to
	// be token0 in about half the pools on this chain, so this was the common
	// case, not the corner.
	side := p.sideWithin2Pct(s)

	baseBal, quoteBal := s.Balance0, s.Balance1
	if p.QuoteIsToken0 {
		baseBal, quoteBal = s.Balance1, s.Balance0
	}

	// Upward: paid out in base, so bounded by the base reserve valued in quote.
	upCap := new(big.Int).Mul(orZero(baseBal), p.Price(s))
	upCap.Quo(upCap, oneE18)
	upCap = rescale(upCap, p.baseDecimals(), p.quoteDecimals())
	// Downward: paid out in quote, bounded by the quote reserve.
	downCap := orZero(quoteBal)

	d := new(big.Int).Add(min2(side, upCap), min2(side, downCap))

	if p.DepthHaircutBps > 0 {
		d.Mul(d, big.NewInt(health.BPS-p.DepthHaircutBps))
		d.Quo(d, big.NewInt(health.BPS))
	}
	return scaleDown(d, p.quoteDecimals())
}

// RelativeDepth is the unbounded constant-liquidity estimate, for weighting one
// pool against another and for nothing else.
//
// Two functions rather than one because they answer different questions and
// only one of them is dangerous to get wrong.
//
// The aggregator wants a *relative* weight: among the pools pricing this pair,
// which carries more? A constant-liquidity estimate that overstates every pool
// by a similar factor still ranks them correctly, and the weighted median moves
// barely at all. It is also the only estimate available on that path, which
// reads price and liquidity and skips balances because doing otherwise doubles
// the round trips and gets venues dropped on their timeout.
//
// DepthWithin2Pct answers an *absolute* question -- how much can actually be
// executed -- and there an overstatement sizes real exposure against liquidity
// that does not exist. That one is bounded by the reserves and needs them.
//
// Never size anything from this. It reported 156x a pool's entire balance on
// Robinhood Chain's PONS/WETH.
func (p V3Pool) RelativeDepth(s *V3State) *big.Int {
	d := p.sideWithin2Pct(s)
	d.Lsh(d, 1)
	if p.DepthHaircutBps > 0 {
		d.Mul(d, big.NewInt(health.BPS-p.DepthHaircutBps))
		d.Quo(d, big.NewInt(health.BPS))
	}
	return scaleDown(d, p.quoteDecimals())
}

func min2(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return new(big.Int).Set(a)
	}
	return new(big.Int).Set(b)
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

func (p V3Pool) quoteDecimals() uint8 {
	if p.QuoteIsToken0 {
		return p.Decimals0
	}
	return p.Decimals1
}

func (p V3Pool) baseDecimals() uint8 {
	if p.QuoteIsToken0 {
		return p.Decimals1
	}
	return p.Decimals0
}

// Price is quote per base, in 1e18 fixed point.
//
// slot0 reports token1/token0 in raw units, so the decimal difference between
// the two sides has to be applied or an 18/6-decimal pair reads twelve orders
// of magnitude off.
func (p V3Pool) Price(s *V3State) *big.Int {
	// (sqrtP/2^96)^2 * 1e18, then adjust for decimals.
	num := new(big.Int).Mul(s.SqrtPriceX96, s.SqrtPriceX96)
	num.Mul(num, oneE18)
	den := new(big.Int).Mul(two96, two96)
	price := num.Quo(num, den) // token1 per token0, raw, 1e18 scaled

	if p.QuoteIsToken0 {
		// Invert: token0 per token1.
		if price.Sign() == 0 {
			return big.NewInt(0)
		}
		inv := new(big.Int).Mul(oneE18, oneE18)
		price = inv.Quo(inv, price)
	}
	// Raw price carries 10^(baseDecimals - quoteDecimals).
	bd, qd := int64(p.baseDecimals()), int64(p.quoteDecimals())
	if diff := bd - qd; diff > 0 {
		price.Mul(price, new(big.Int).Exp(big.NewInt(10), big.NewInt(diff), nil))
	} else if diff < 0 {
		price.Quo(price, new(big.Int).Exp(big.NewInt(10), big.NewInt(-diff), nil))
	}
	return price
}

// TVL is the pool's holdings valued in quote units.
func (p V3Pool) TVL(s *V3State) *big.Int {
	baseBal, quoteBal := s.Balance0, s.Balance1
	if !p.QuoteIsToken0 {
		baseBal, quoteBal = s.Balance0, s.Balance1
	} else {
		baseBal, quoteBal = s.Balance1, s.Balance0
	}
	price := p.Price(s) // 1e18-scaled quote per base

	baseValue := new(big.Int).Mul(baseBal, price)
	baseValue.Quo(baseValue, oneE18)
	// baseValue is now in base-decimal units of quote; rescale.
	baseValue = rescale(baseValue, p.baseDecimals(), p.quoteDecimals())

	total := new(big.Int).Add(baseValue, quoteBal)
	return scaleDown(total, p.quoteDecimals())
}

// ObserveV3 builds a health observation from a concentrated-liquidity pool.
func (o *Observer) ObserveV3(ctx context.Context, p V3Pool, now time.Time) (health.Observation, *V3State, error) {
	var obs health.Observation
	obs.Time = now

	s, err := o.ReadV3(ctx, p)
	if err != nil {
		return obs, nil, err
	}
	obs.DepthWithin2Pct = toInt64(p.DepthWithin2Pct(s))
	obs.SpotTVL = toInt64(p.TVL(s))
	obs.TopPoolShareBps = health.BPS

	if p.BaseSupply != nil && p.BaseSupply.Sign() > 0 {
		mcap := new(big.Int).Mul(p.BaseSupply, p.Price(s))
		mcap.Quo(mcap, oneE18)
		obs.MarketCap = toInt64(scaleDown(mcap, p.baseDecimals()))
	}
	return obs, s, nil
}

func scaleDown(v *big.Int, decimals uint8) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return new(big.Int).Quo(v, d)
}

func rescale(v *big.Int, from, to uint8) *big.Int {
	if from == to {
		return v
	}
	if from > to {
		d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(from-to)), nil)
		return new(big.Int).Quo(v, d)
	}
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(to-from)), nil)
	return new(big.Int).Mul(v, d)
}

func toInt64(v *big.Int) int64 {
	if v == nil || v.Sign() <= 0 {
		return 0
	}
	if !v.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return v.Int64()
}
