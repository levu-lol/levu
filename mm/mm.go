// Package mm scores market makers and sets what they are paid.
//
// A matching engine with no resting orders is a very fast way to do nothing, so
// the terms offered to whoever provides them are not a detail. The question is
// what to pay *for*, and the honest answer is not volume: volume is trivially
// self-dealt, and a rebate schedule keyed to it pays people to trade with
// themselves.
//
// What a market actually needs is quotes that are present when someone wants to
// trade and tight enough to be worth taking. So the score is built from uptime
// and spread, with volume as a floor rather than a driver — necessary to
// qualify, never the thing being rewarded. It is the same reasoning as the
// health engine's treatment of cheap signals, applied to a different party.
package mm

import (
	"fmt"
	"sort"
	"time"

	"github.com/levu-lol/levu/wire"
)

// BPS is the fixed-point scale, matching the health engine.
const BPS = 10_000

// Activity is one maker's measured behaviour over a scoring window.
type Activity struct {
	Account wire.Account
	Name    string

	// TwoSidedMillis is how long the maker had quotes on both sides. One-sided
	// quoting is not making a market, it is taking a view.
	TwoSidedMillis int64
	// WindowMillis is the length of the measurement window.
	WindowMillis int64
	// AverageSpreadBps of the maker's own quotes while two-sided.
	AverageSpreadBps int64
	// DepthAtBest is the quote size the maker showed at the top of book, in
	// quote units, averaged over the window.
	DepthAtBest int64

	// MakerVolume filled against the maker's resting orders. A qualifying floor
	// only — see the package comment.
	MakerVolume int64
	// SelfMatched volume, which the VM's self-trade prevention already refuses.
	// Any observed here means the maker was trying.
	SelfMatched int64
}

// Uptime is the fraction of the window spent quoting both sides, in bps.
func (a Activity) UptimeBps() int64 {
	if a.WindowMillis <= 0 {
		return 0
	}
	u := a.TwoSidedMillis * BPS / a.WindowMillis
	if u > BPS {
		return BPS
	}
	return u
}

// Tier is a rebate band.
type Tier struct {
	Name string
	// MinScore to qualify.
	MinScore int64
	// MakerFeeBps is what the maker pays, and may be negative — a rebate.
	MakerFeeBps int64
}

// Config tunes scoring and the schedule.
type Config struct {
	// Qualifying floors. Failing any means no programme at all.
	MinVolume      int64
	MinUptimeBps   int64
	MinDepthAtBest int64
	// TargetSpreadBps is the spread at which the spread component saturates.
	TargetSpreadBps int64
	// TargetDepth at which the depth component saturates.
	TargetDepth int64

	// Weights, in bps, summing to BPS.
	WeightUptime int64
	WeightSpread int64
	WeightDepth  int64

	// Tiers, ordered by descending MinScore.
	Tiers []Tier
}

func DefaultConfig() Config {
	return Config{
		MinVolume:      100_000,
		MinUptimeBps:   5_000, // present half the time
		MinDepthAtBest: 5_000,

		TargetSpreadBps: 10, // ten basis points is a tight quote
		TargetDepth:     100_000,

		WeightUptime: 5_000,
		WeightSpread: 3_000,
		WeightDepth:  2_000,

		Tiers: []Tier{
			{Name: "primary", MinScore: 8_000, MakerFeeBps: -2},
			{Name: "designated", MinScore: 6_000, MakerFeeBps: -1},
			{Name: "standard", MinScore: 4_000, MakerFeeBps: 0},
		},
	}
}

func (c Config) Validate() error {
	if sum := c.WeightUptime + c.WeightSpread + c.WeightDepth; sum != BPS {
		return fmt.Errorf("mm: weights sum to %d, want %d", sum, BPS)
	}
	for i := 1; i < len(c.Tiers); i++ {
		if c.Tiers[i].MinScore >= c.Tiers[i-1].MinScore {
			return fmt.Errorf("mm: tiers must descend by score")
		}
	}
	return nil
}

// Score is one maker's assessment.
type Score struct {
	Account wire.Account
	Name    string
	Total   int64
	Uptime  int64
	Spread  int64
	Depth   int64
	// Tier the maker qualifies for, empty when none.
	Tier string
	// MakerFeeBps the maker should be charged, negative for a rebate.
	MakerFeeBps int64
	// Disqualified lists floors the maker failed.
	Disqualified []string
}

func (s Score) Qualified() bool { return len(s.Disqualified) == 0 && s.Tier != "" }

func ratio(v, target int64) int64 {
	if target <= 0 || v <= 0 {
		return 0
	}
	if v >= target {
		return BPS
	}
	return v * BPS / target
}

// Assess scores one maker.
func Assess(a Activity, cfg Config) Score {
	s := Score{Account: a.Account, Name: a.Name}

	// Self-matching is refused by the VM, so seeing any means the maker tried
	// to manufacture the very signal the programme declines to reward.
	// Disqualifying outright is cheaper than pricing it.
	if a.SelfMatched > 0 {
		s.Disqualified = append(s.Disqualified, "attempted self-matching")
	}
	if a.MakerVolume < cfg.MinVolume {
		s.Disqualified = append(s.Disqualified, "maker volume below floor")
	}
	if a.UptimeBps() < cfg.MinUptimeBps {
		s.Disqualified = append(s.Disqualified, "two-sided uptime below floor")
	}
	if a.DepthAtBest < cfg.MinDepthAtBest {
		s.Disqualified = append(s.Disqualified, "quoted depth below floor")
	}

	s.Uptime = a.UptimeBps()
	// Tighter is better, so the spread component inverts: at or inside the
	// target it saturates, and it decays as quotes widen.
	if a.AverageSpreadBps <= 0 {
		s.Spread = 0
	} else if a.AverageSpreadBps <= cfg.TargetSpreadBps {
		s.Spread = BPS
	} else {
		s.Spread = cfg.TargetSpreadBps * BPS / a.AverageSpreadBps
	}
	s.Depth = ratio(a.DepthAtBest, cfg.TargetDepth)

	s.Total = (s.Uptime*cfg.WeightUptime +
		s.Spread*cfg.WeightSpread +
		s.Depth*cfg.WeightDepth) / BPS

	if len(s.Disqualified) > 0 {
		return s
	}
	for _, t := range cfg.Tiers {
		if s.Total >= t.MinScore {
			s.Tier, s.MakerFeeBps = t.Name, t.MakerFeeBps
			break
		}
	}
	return s
}

// AssessAll scores a set of makers, ordered by score then name so the output is
// reproducible.
func AssessAll(activity []Activity, cfg Config) []Score {
	out := make([]Score, 0, len(activity))
	for _, a := range activity {
		out = append(out, Assess(a, cfg))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// MakerFee renders a tier's fee as the VM parameter.
func MakerFee(bps int64) wire.Fixed {
	return wire.FixedWhole(bps).Div(wire.FixedWhole(BPS))
}

// Window is a convenience for building activity from a duration.
func Window(d time.Duration) int64 { return d.Milliseconds() }
