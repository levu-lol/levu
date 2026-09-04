// Package indexer derives market observations from on-chain state.
//
// This is what replaces the scenario replay: the health engine's inputs stop
// being a fixture and start being what a pool actually holds. Everything here
// reads a constant-product pair, because that is what long-tail liquidity
// mostly is — a single pool whose reserves are the whole story.
//
// What is derivable from a pool alone, and what is not, is worth being precise
// about. Reserves give price, executable depth and TVL exactly. Total supply
// gives market cap. Swap logs give volume. Holder concentration does *not*
// come from a pool and needs a token-transfer index, so it is reported as
// unknown rather than guessed — and the health engine treats unknown
// concentration as disqualifying, which is the safe direction.
package indexer

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/levu-lol/levu/health"
)

// Chain reads contract state. Implemented over `cast` here; a production
// deployment would use an RPC client, and nothing above this changes.
type Chain interface {
	// Call executes a read-only contract call and returns the decoded words.
	Call(ctx context.Context, to, sig string, args ...string) ([]string, error)
	// Logs returns matching event data over a block range.
	Logs(ctx context.Context, addr, topic string, fromBlock, toBlock int64) ([]Log, error)
	// BlockNumber is the current head.
	BlockNumber(ctx context.Context) (int64, error)
}

// Log is one decoded event occurrence.
type Log struct {
	BlockNumber int64
	Data        []byte
}

// Pool describes a market to observe.
type Pool struct {
	Symbol string
	// Address of the constant-product pair.
	Pair string
	// Address of the base token, whose supply sets market cap.
	BaseToken string
	// True when the quote asset is token0 rather than token1.
	QuoteIsToken0 bool
	// Block the pool was created in, for age.
	CreatedBlock int64
	// Seconds per block, for converting block distance into age.
	BlockSeconds int64
}

// depthFractionWithin2Pct is the share of the quote reserve that can be traded
// before the price moves 2%.
//
// For a constant-product pool, moving price by d requires trading the reserve
// down by a factor of 1/sqrt(1+d), so the tradeable quote value is
// `reserve * (1 - 1/sqrt(1+d))`. At d = 0.02 that is 0.985% of the reserve per
// side. This is the honest number: a $10M pool does *not* have $10M of
// executable depth, it has about $197k within 2%, and a listing rule that
// confuses the two grants leverage no book can support.
var depthFractionWithin2Pct = 1 - 1/math.Sqrt(1.02)

// Observer turns pool state into health observations.
type Observer struct {
	chain Chain
	// VolumeWindow is how far back swap logs are summed.
	VolumeWindow time.Duration
}

func New(c Chain) *Observer {
	return &Observer{chain: c, VolumeWindow: 24 * time.Hour}
}

// Reserves reads a pair's reserves, oriented as (base, quote).
func (o *Observer) Reserves(ctx context.Context, p Pool) (base, quote *big.Int, err error) {
	out, err := o.chain.Call(ctx, p.Pair, "getReserves()(uint112,uint112,uint32)")
	if err != nil {
		return nil, nil, fmt.Errorf("getReserves: %w", err)
	}
	if len(out) < 2 {
		return nil, nil, fmt.Errorf("getReserves returned %d values", len(out))
	}
	r0, ok0 := new(big.Int).SetString(out[0], 10)
	r1, ok1 := new(big.Int).SetString(out[1], 10)
	if !ok0 || !ok1 {
		return nil, nil, fmt.Errorf("could not parse reserves %q, %q", out[0], out[1])
	}
	if p.QuoteIsToken0 {
		return r1, r0, nil
	}
	return r0, r1, nil
}

// Observe builds a health observation from chain state.
//
// Fields a pool cannot answer are left at their zero value rather than filled
// with a plausible guess. The health engine gates on several of them, so an
// unknown reads as a refusal — which is the direction an unknown should push a
// decision about granting leverage.
func (o *Observer) Observe(ctx context.Context, p Pool, now time.Time) (health.Observation, error) {
	var obs health.Observation
	obs.Time = now

	_, quote, err := o.Reserves(ctx, p)
	if err != nil {
		return obs, err
	}
	quoteUnits := toUnits(quote)

	// Both sides of the book are tradeable, so executable depth within 2% is
	// twice the one-sided figure.
	obs.DepthWithin2Pct = int64(float64(quoteUnits) * depthFractionWithin2Pct * 2)
	// A constant-product pool holds equal value on each side.
	obs.SpotTVL = quoteUnits * 2

	head, err := o.chain.BlockNumber(ctx)
	if err != nil {
		return obs, fmt.Errorf("block number: %w", err)
	}
	if p.CreatedBlock > 0 && p.BlockSeconds > 0 && head >= p.CreatedBlock {
		obs.Age = time.Duration(head-p.CreatedBlock) * time.Duration(p.BlockSeconds) * time.Second
	}

	if p.BaseToken != "" {
		supply, err := o.chain.Call(ctx, p.BaseToken, "totalSupply()(uint256)")
		if err == nil && len(supply) > 0 {
			if s, ok := new(big.Int).SetString(supply[0], 10); ok {
				base, _, rerr := o.Reserves(ctx, p)
				if rerr == nil && base.Sign() > 0 {
					// price = quote/base, mcap = supply * price
					mcap := new(big.Int).Mul(s, quote)
					mcap.Quo(mcap, base)
					obs.MarketCap = toUnits(mcap)
				}
			}
		}
	}

	// Swap volume over the window. Each Swap carries four amounts; the quote
	// legs are what count, and summing both in and out double-counts a trade,
	// so only the outbound quote leg is taken.
	if o.VolumeWindow > 0 && p.BlockSeconds > 0 {
		blocks := int64(o.VolumeWindow.Seconds()) / p.BlockSeconds
		from := head - blocks
		if from < 0 {
			from = 0
		}
		vol, err := o.swapVolume(ctx, p, from, head)
		if err == nil {
			obs.Volume24h = vol
		}
	}

	// A single pool is, trivially, all of the liquidity.
	obs.TopPoolShareBps = health.BPS
	// Holder concentration is not visible from a pool. Left unknown, which the
	// engine reads as fully dispersed — so a caller that cannot supply it must
	// gate on it separately rather than assume this is safe.
	return obs, nil
}

// swapVolume sums the quote-denominated outflow of Swap events.
func (o *Observer) swapVolume(ctx context.Context, p Pool, from, to int64) (int64, error) {
	// keccak256("Swap(address,uint256,uint256,uint256,uint256,address)")
	const swapTopic = "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822"
	logs, err := o.chain.Logs(ctx, p.Pair, swapTopic, from, to)
	if err != nil {
		return 0, err
	}
	total := new(big.Int)
	for _, l := range logs {
		// Data is four 32-byte words: amount0In, amount1In, amount0Out, amount1Out.
		if len(l.Data) < 128 {
			continue
		}
		out0 := new(big.Int).SetBytes(l.Data[64:96])
		out1 := new(big.Int).SetBytes(l.Data[96:128])
		if p.QuoteIsToken0 {
			total.Add(total, out0)
		} else {
			total.Add(total, out1)
		}
	}
	return toUnits(total), nil
}

var wad = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// toUnits converts an 18-decimal token amount to whole quote units, which is
// what the health engine works in.
func toUnits(v *big.Int) int64 {
	if v == nil || v.Sign() <= 0 {
		return 0
	}
	q := new(big.Int).Quo(v, wad)
	if !q.IsInt64() {
		return math.MaxInt64
	}
	return q.Int64()
}
