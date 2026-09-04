// Package oracle aggregates price observations into an index and a confidence.
//
// The output is a VM *input*, recorded in the intent stream, so replay reads
// the recorded value rather than recomputing it. That means this code need not
// match the VM's arithmetic bit for bit — but it must be deterministic in Go,
// or the same observations produce different prices run to run and nobody can
// reproduce a disputed reading.
//
// The design assumption throughout: on a long-tail asset, the venue that is
// easiest to manipulate is also the thinnest. So price is a *liquidity-weighted*
// median, never a mean and never an unweighted median — a venue with $2k of
// depth must not be able to outvote one with $2M.
package oracle

import (
	"fmt"
	"sort"
	"time"

	"github.com/levu-lol/levu/wire"
)

// Source is one venue's observation.
type Source struct {
	Name string
	// Price in quote units.
	Price wire.Fixed
	// Executable depth backing this quote. This is the weight, and it is the
	// whole defence: manipulation cost scales with it.
	Liquidity wire.Fixed
	// When the observation was taken.
	Observed time.Time
}

// Config tunes filtering and the confidence formula.
type Config struct {
	// Observations older than this are discarded.
	MaxAge time.Duration
	// Fractional distance from the provisional median beyond which a source is
	// discarded as an outlier. 0.05 drops anything more than 5% away.
	MaxDeviation wire.Fixed
	// Below this many surviving sources the reading is unusable at any
	// confidence.
	MinSources int
	// Source count at which coverage stops improving confidence.
	TargetSources int
	// Total liquidity at which depth stops improving confidence.
	TargetLiquidity wire.Fixed
	// Liquidity at which a single source counts as one full, independent
	// observation for coverage purposes. Below it, a source counts
	// proportionally less — otherwise an adversary raises coverage by standing
	// up venues with no depth behind them.
	MinSourceLiquidity wire.Fixed
}

// DefaultConfig is a starting point for a long-tail crypto lane. These numbers
// are not calibrated against real market data — see the health engine's
// backtest harness for how to calibrate them.
func DefaultConfig() Config {
	return Config{
		MaxAge:             30 * time.Second,
		MaxDeviation:       wire.FixedRawInt64(50_000_000_000_000_000), // 0.05
		MinSources:         2,
		TargetSources:      4,
		TargetLiquidity:    wire.FixedWhole(1_000_000),
		MinSourceLiquidity: wire.FixedWhole(250_000),
	}
}

// Why a source was discarded.
type Rejection struct {
	Name   string
	Reason string
}

// Result is an aggregated reading.
type Result struct {
	Price wire.Fixed
	// Basis points, 0..10000.
	Confidence uint16
	// Healthy is false when the reading must not be used at all — as distinct
	// from a usable reading with low confidence, which the VM will accept for
	// closing but not for opening.
	Healthy bool
	Used    []string
	Reject  []Rejection
	// Components of the confidence figure, retained so a low reading can be
	// explained rather than merely observed.
	Coverage  wire.Fixed
	Depth     wire.Fixed
	Agreement wire.Fixed
}

const confidenceMax = 10_000

// Aggregate reduces observations to an index price and a confidence.
func Aggregate(sources []Source, cfg Config, now time.Time) Result {
	var res Result

	// 1. Discard observations that cannot be used at all.
	live := make([]Source, 0, len(sources))
	for _, s := range sources {
		switch {
		case !s.Price.IsPositive():
			res.Reject = append(res.Reject, Rejection{s.Name, "non-positive price"})
		case !s.Liquidity.IsPositive():
			res.Reject = append(res.Reject, Rejection{s.Name, "no liquidity behind quote"})
		case cfg.MaxAge > 0 && now.Sub(s.Observed) > cfg.MaxAge:
			res.Reject = append(res.Reject, Rejection{
				s.Name,
				fmt.Sprintf("stale by %v", now.Sub(s.Observed).Round(time.Millisecond)),
			})
		default:
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return res
	}

	// 2. Provisional median, used only to locate outliers.
	provisional := weightedMedian(live)

	// 3. Drop sources too far from it. Done against the median rather than the
	//    mean precisely so that one extreme quote cannot drag the reference it
	//    is being judged against.
	kept := make([]Source, 0, len(live))
	for _, s := range live {
		dev := s.Price.Sub(provisional).Abs().Div(provisional)
		if cfg.MaxDeviation.IsPositive() && dev.Cmp(cfg.MaxDeviation) > 0 {
			res.Reject = append(res.Reject, Rejection{
				s.Name, fmt.Sprintf("deviates %s from median", dev),
			})
			continue
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return res
	}

	// 4. Final price.
	res.Price = weightedMedian(kept)
	for _, s := range kept {
		res.Used = append(res.Used, s.Name)
	}

	// 5. Confidence: the product of three independent [0,1] factors, so any one
	//    of them collapsing collapses the reading. Multiplicative rather than
	//    averaged on purpose — a deep, unanimous reading from a single venue is
	//    still a single point of failure, and averaging would hide that.
	one := wire.FixedOne()

	total := wire.FixedZero()
	for _, s := range kept {
		total = total.Add(s.Liquidity)
	}

	// Coverage counts *effective* observations, not raw sources.
	//
	// A naive count is adversarially exploitable in two directions: an attacker
	// adds a venue quoting far from the median and coverage rises even as the
	// reading gets worse, or sybils a dozen empty venues at the honest price to
	// manufacture apparent independence. Weighting each source by how closely
	// it agrees *and* by whether it carries real depth removes both, because a
	// source that disagrees or has nothing behind it contributes almost
	// nothing.
	coverage := one
	if cfg.TargetSources > 0 {
		effective := wire.FixedZero()
		for _, s := range kept {
			agree := one
			if cfg.MaxDeviation.IsPositive() {
				d := s.Price.Sub(res.Price).Abs().Div(res.Price)
				agree = one.Sub(d.Div(cfg.MaxDeviation).Min(one))
			}
			weight := one
			if cfg.MinSourceLiquidity.IsPositive() {
				weight = s.Liquidity.Div(cfg.MinSourceLiquidity).Min(one)
			}
			effective = effective.Add(agree.Mul(weight))
		}
		coverage = effective.Div(wire.FixedWhole(int64(cfg.TargetSources))).Min(one)
	}

	depth := one
	if cfg.TargetLiquidity.IsPositive() {
		depth = total.Div(cfg.TargetLiquidity).Min(one)
	}

	// Liquidity-weighted mean absolute deviation from the median, as a fraction
	// of the median: how much the venues that matter disagree.
	agreement := one
	if cfg.MaxDeviation.IsPositive() && total.IsPositive() {
		wmad := wire.FixedZero()
		for _, s := range kept {
			d := s.Price.Sub(res.Price).Abs().Div(res.Price)
			wmad = wmad.Add(d.Mul(s.Liquidity))
		}
		wmad = wmad.Div(total)
		agreement = one.Sub(wmad.Div(cfg.MaxDeviation).Min(one))
	}

	res.Coverage, res.Depth, res.Agreement = coverage, depth, agreement
	conf := coverage.Mul(depth).Mul(agreement).Mul(wire.FixedWhole(confidenceMax))
	res.Confidence = toBps(conf)
	res.Healthy = len(kept) >= cfg.MinSources
	if !res.Healthy {
		res.Reject = append(res.Reject, Rejection{
			"", fmt.Sprintf("%d sources survived, need %d", len(kept), cfg.MinSources),
		})
	}
	return res
}

// weightedMedian returns the lower liquidity-weighted median: the price at
// which cumulative liquidity first reaches half the total.
//
// Ties on price are broken by name so the result never depends on the order
// observations happened to arrive in.
func weightedMedian(src []Source) wire.Fixed {
	s := make([]Source, len(src))
	copy(s, src)
	sort.SliceStable(s, func(i, j int) bool {
		if c := s[i].Price.Cmp(s[j].Price); c != 0 {
			return c < 0
		}
		return s[i].Name < s[j].Name
	})

	total := wire.FixedZero()
	for _, x := range s {
		total = total.Add(x.Liquidity)
	}
	half := total.Div(wire.FixedWhole(2))

	cum := wire.FixedZero()
	for _, x := range s {
		cum = cum.Add(x.Liquidity)
		if cum.Cmp(half) >= 0 {
			return x.Price
		}
	}
	return s[len(s)-1].Price
}

// toBps clamps a WAD figure into the basis-point range.
func toBps(f wire.Fixed) uint16 {
	if f.Sign() <= 0 {
		return 0
	}
	v := f.BigInt()
	v.Quo(v, wire.WadInt())
	if !v.IsInt64() || v.Int64() > confidenceMax {
		return confidenceMax
	}
	return uint16(v.Int64())
}
