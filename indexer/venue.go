package indexer

import (
	"context"
	"fmt"
	"math/big"

	"github.com/levu-lol/levu/oracle"
	"github.com/levu-lol/levu/wire"
)

// PoolVenue reads a concentrated-liquidity pool as an oracle source.
//
// The liquidity it reports is executable depth within 2%, not total value
// locked. That distinction is what makes the aggregator's weighting mean
// something: a pool holding ten million dollars across a wide range is a
// thinner price source than one holding two million concentrated at the mark,
// and weighting by TVL would get that backwards.
type PoolVenue struct {
	Pool V3Pool
	Obs  *Observer
	// Label overrides the venue name in aggregation output.
	Label string
}

func (p PoolVenue) Name() string {
	if p.Label != "" {
		return p.Label
	}
	if len(p.Pool.Pool) >= 10 {
		return "pool:" + p.Pool.Pool[:10]
	}
	return "pool"
}

func (p PoolVenue) Quote(ctx context.Context) (oracle.Source, error) {
	// Price and depth only; balances are the health engine's business.
	s, err := p.Obs.ReadV3Price(ctx, p.Pool)
	if err != nil {
		return oracle.Source{}, fmt.Errorf("%s: %w", p.Name(), err)
	}
	price := p.Pool.Price(s)
	if price == nil || price.Sign() <= 0 {
		return oracle.Source{}, fmt.Errorf("%s: non-positive price", p.Name())
	}
	// RelativeDepth, not DepthWithin2Pct: this is a weight for ranking pools
	// against each other, and this path deliberately does not read balances, so
	// the bounded figure would be zero for every venue. Using it here once cost
	// a running exchange every one of its price sources.
	depth := p.Pool.RelativeDepth(s)
	if depth == nil || depth.Sign() <= 0 {
		// A pool with no liquidity has a price but no evidence behind it.
		// Reporting it with zero weight would be harmless; reporting it as a
		// failure is honest, and keeps the coverage count truthful.
		return oracle.Source{}, fmt.Errorf("%s: no liquidity behind the price", p.Name())
	}
	return oracle.Source{
		Name:      p.Name(),
		Price:     wire.FixedRaw(price),
		Liquidity: wire.FixedRaw(new(big.Int).Mul(depth, wadInt())),
	}, nil
}

func wadInt() *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
}

// PoolVenues builds a venue per fee tier that actually holds liquidity.
//
// Fee tiers are independent pools with independent depth, so each is its own
// price source rather than one venue averaged in advance — which is exactly the
// independence the aggregator's coverage term is counting.
func PoolVenues(ctx context.Context, o *Observer, chain Chain, factory, base, quote string,
	baseDec, quoteDec uint8, haircutBps int64) ([]oracle.Venue, error) {

	var out []oracle.Venue
	for _, fee := range []string{"100", "500", "3000", "10000"} {
		res, err := chain.Call(ctx, factory, "getPool(address,address,uint24)(address)",
			base, quote, fee)
		if err != nil || len(res) == 0 {
			continue
		}
		pool := res[0]
		if isZeroAddress(pool) {
			continue
		}
		t0, err := chain.Call(ctx, pool, "token0()(address)")
		if err != nil || len(t0) == 0 {
			continue
		}
		quoteIsToken0 := equalFold(t0[0], quote)

		p := V3Pool{
			Symbol: base, Pool: pool,
			QuoteIsToken0:   quoteIsToken0,
			DepthHaircutBps: haircutBps,
		}
		if quoteIsToken0 {
			p.Token0, p.Token1 = quote, base
			p.Decimals0, p.Decimals1 = quoteDec, baseDec
		} else {
			p.Token0, p.Token1 = base, quote
			p.Decimals0, p.Decimals1 = baseDec, quoteDec
		}
		out = append(out, PoolVenue{Pool: p, Obs: o, Label: "uniswap-" + fee})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no pools found for %s/%s", base, quote)
	}
	return out, nil
}

func isZeroAddress(a string) bool {
	return equalFold(a, "0x0000000000000000000000000000000000000000")
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 32
		}
		if 'A' <= y && y <= 'Z' {
			y += 32
		}
		if x != y {
			return false
		}
	}
	return true
}
