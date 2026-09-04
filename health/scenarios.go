package health

import "time"

// Synthetic market histories encoding known failure modes.
//
// These validate that the engine does the right thing when handed a shape it
// should recognise. They do NOT calibrate it: whether 75 is the right listing
// threshold for Robinhood Chain is a question only real observation series can
// answer, and Run/RunAll take those just as readily.

var epoch = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func at(h int) time.Time { return epoch.Add(time.Duration(h) * time.Hour) }

// series builds observations at hourly intervals from a template mutated per
// step, so a scenario reads as a trajectory rather than a wall of literals.
func series(hours int, f func(h int, o *Observation)) []Observation {
	out := make([]Observation, 0, hours)
	for h := 0; h < hours; h++ {
		o := Observation{Time: at(h)}
		f(h, &o)
		// Scenarios describe *established* lanes, so unless one says otherwise
		// they carry a book. A real perp book is thinner than the AMM behind
		// it, so a third is the assumption, and it is stated here rather than
		// left implied by a zero -- a zero would now mean "cannot be closed at
		// all", which is a very different scenario from the one being written.
		if o.BookDepthWithin2Pct == 0 {
			o.BookDepthWithin2Pct = o.DepthWithin2Pct / 3
		}
		out = append(out, o)
	}
	return out
}

// Scenarios is the standard suite.
func Scenarios() []Scenario {
	return []Scenario{
		healthyMajor(),
		honestMeme(),
		washTraded(),
		tooYoung(),
		concentrated(),
		gradualRug(),
		atomicRug(),
		oracleOutage(),
		slowBleed(),
		coldStart(),
	}
}

// The launch case, and the one the old engine got wrong.
//
// A well-known token with deep spot liquidity, listed on a lane whose own book
// is empty because nobody has quoted it yet. Every venue signal is excellent.
// Nothing can be closed.
//
// The old engine read Uniswap, saw millions, and would have granted top-tier
// leverage against a book that could not fill a single order. It should list --
// the market is real -- but at 1x until somebody makes a market in it.
func coldStart() Scenario {
	return Scenario{
		Name:        "cold-start",
		Description: "deep spot, empty book: real market, no liquidity of ours yet",
		ShouldList:  true,
		Obs: series(48, func(h int, o *Observation) {
			o.DepthWithin2Pct = 4_000_000
			o.UnderwrittenCapital = 250_000
			o.Age = 200 * 24 * time.Hour
			o.TopHolderShareBps = 700
			o.TopPoolShareBps = 4_000
			o.OracleConfidence = 9_200
			o.Volume24h = 20_000_000
			o.SpotTVL = 9_000_000
			o.MarketCap = 120_000_000
			o.RealisedVolBps = 9_000
			// Our book fills in slowly as makers arrive.
			switch {
			case h < 24:
				o.BookDepthWithin2Pct = 1 // effectively nothing; a zero would be filled in
			default:
				o.BookDepthWithin2Pct = int64(h-23) * 60_000
			}
		}),
	}
}

// A deep, mature, well-covered market. Should list at the top tier and stay.
func healthyMajor() Scenario {
	return Scenario{
		Name:        "healthy-major",
		Description: "deep book, mature token, several venues agreeing",
		ShouldList:  true,
		Obs: series(48, func(h int, o *Observation) {
			o.DepthWithin2Pct = 3_000_000
			o.UnderwrittenCapital = 800_000
			o.Age = 200 * 24 * time.Hour
			o.TopHolderShareBps = 500
			o.TopPoolShareBps = 3_000
			o.OracleConfidence = 9_500
			o.Volume24h = 40_000_000
			o.SpotTVL = 12_000_000
			o.MarketCap = 400_000_000
			o.RealisedVolBps = 6_000
			o.OpenInterest = 500_000
		}),
	}
}

// A young but genuinely liquid memecoin: the target customer. Should list, but
// at a low tier and a small cap.
func honestMeme() Scenario {
	return Scenario{
		Name:        "honest-meme",
		Description: "three days old, real depth, real holders, volatile",
		ShouldList:  true,
		Obs: series(48, func(h int, o *Observation) {
			o.DepthWithin2Pct = 700_000
			o.UnderwrittenCapital = 150_000
			o.Age = time.Duration(72+h) * time.Hour
			o.TopHolderShareBps = 1_200
			o.TopPoolShareBps = 6_000
			o.OracleConfidence = 7_500
			o.Volume24h = 4_000_000
			o.SpotTVL = 1_800_000
			o.MarketCap = 25_000_000
			o.RealisedVolBps = 22_000
			o.OpenInterest = 100_000
		}),
	}
}

// Enormous printed volume, no executable depth, supply in one wallet. The case
// that a volume-and-market-cap listing rule waves straight through.
func washTraded() Scenario {
	return Scenario{
		Name:        "wash-traded",
		Description: "$80M of printed volume behind $30k of real depth",
		ShouldList:  false,
		Obs: series(48, func(h int, o *Observation) {
			o.DepthWithin2Pct = 30_000
			o.UnderwrittenCapital = 0
			o.Age = 30 * time.Hour
			o.TopHolderShareBps = 7_800
			o.TopPoolShareBps = 9_500
			o.OracleConfidence = 3_000
			o.Volume24h = 80_000_000
			o.SpotTVL = 400_000
			o.MarketCap = 90_000_000
			o.RealisedVolBps = 35_000
			o.OpenInterest = 0
		}),
	}
}

// Everything looks excellent, but the token is two hours old.
func tooYoung() Scenario {
	return Scenario{
		Name:        "too-young",
		Description: "strong on every metric, two hours old",
		ShouldList:  false,
		Obs: series(20, func(h int, o *Observation) {
			o.DepthWithin2Pct = 2_500_000
			o.UnderwrittenCapital = 400_000
			o.Age = time.Duration(2+h) * time.Hour
			o.TopHolderShareBps = 900
			o.TopPoolShareBps = 4_000
			o.OracleConfidence = 8_500
			o.Volume24h = 20_000_000
			o.SpotTVL = 6_000_000
			o.MarketCap = 60_000_000
			o.RealisedVolBps = 18_000
			o.OpenInterest = 0
		}),
	}
}

// Deep and liquid, but 82% of supply sits in one wallet: a market one
// participant can end whenever they choose.
func concentrated() Scenario {
	return Scenario{
		Name:        "concentrated",
		Description: "deep book, but one wallet holds 82% of supply",
		ShouldList:  false,
		Obs: series(48, func(h int, o *Observation) {
			o.DepthWithin2Pct = 1_500_000
			o.UnderwrittenCapital = 100_000
			o.Age = 45 * 24 * time.Hour
			o.TopHolderShareBps = 8_200
			o.TopPoolShareBps = 9_000
			o.OracleConfidence = 7_000
			o.Volume24h = 8_000_000
			o.SpotTVL = 3_000_000
			o.MarketCap = 50_000_000
			o.RealisedVolBps = 25_000
			o.OpenInterest = 0
		}),
	}
}

// Liquidity is withdrawn over several hours before the collapse. Detectable,
// and the engine should stop new exposure with time to spare.
func gradualRug() Scenario {
	collapse := at(40)
	return Scenario{
		Name:        "gradual-rug",
		Description: "liquidity withdrawn over 12 hours, then collapse",
		ShouldList:  true,
		CollapseAt:  collapse,
		Obs: series(48, func(h int, o *Observation) {
			depth := int64(1_200_000)
			conf := int64(8_000)
			if h >= 28 {
				// Depth bleeds away from hour 28 onward.
				drain := int64(h-28) * 95_000
				depth -= drain
				if depth < 20_000 {
					depth = 20_000
				}
				conf -= int64(h-28) * 350
				if conf < 1_000 {
					conf = 1_000
				}
			}
			o.DepthWithin2Pct = depth
			o.UnderwrittenCapital = 200_000
			o.Age = time.Duration(120+h) * time.Hour
			o.TopHolderShareBps = 2_500
			o.TopPoolShareBps = 7_000
			o.OracleConfidence = conf
			o.Volume24h = 6_000_000
			o.SpotTVL = depth * 3
			o.MarketCap = 40_000_000
			o.RealisedVolBps = 20_000
			o.OpenInterest = 200_000
		}),
	}
}

// The LP is pulled in a single block. No detector leads this; the only defence
// is how much exposure was outstanding, which is the OI cap.
func atomicRug() Scenario {
	collapse := at(30)
	return Scenario{
		Name:        "atomic-rug",
		Description: "healthy until the LP is pulled in one block",
		ShouldList:  true,
		CollapseAt:  collapse,
		Atomic:      true,
		Obs: series(48, func(h int, o *Observation) {
			depth := int64(900_000)
			conf := int64(7_800)
			if h >= 30 {
				depth = 5_000
				conf = 500
			}
			o.DepthWithin2Pct = depth
			o.UnderwrittenCapital = 150_000
			o.Age = time.Duration(150+h) * time.Hour
			o.TopHolderShareBps = 3_000
			o.TopPoolShareBps = 8_000
			o.OracleConfidence = conf
			o.Volume24h = 5_000_000
			o.SpotTVL = depth * 3
			o.MarketCap = 30_000_000
			o.RealisedVolBps = 21_000
			o.OpenInterest = 150_000
		}),
	}
}

// The book stays deep but the oracle degrades: venues drop out and disagree.
// The market should stop opening exposure even though liquidity looks fine.
func oracleOutage() Scenario {
	return Scenario{
		Name:        "oracle-outage",
		Description: "book stays deep, price sources fall away",
		ShouldList:  true,
		Obs: series(48, func(h int, o *Observation) {
			conf := int64(9_000)
			if h >= 24 {
				conf = 9_000 - int64(h-24)*700
				if conf < 500 {
					conf = 500
				}
			}
			o.DepthWithin2Pct = 2_000_000
			o.UnderwrittenCapital = 400_000
			o.Age = 90 * 24 * time.Hour
			o.TopHolderShareBps = 1_000
			o.TopPoolShareBps = 5_000
			o.OracleConfidence = conf
			o.Volume24h = 15_000_000
			o.SpotTVL = 8_000_000
			o.MarketCap = 100_000_000
			o.RealisedVolBps = 12_000
			o.OpenInterest = 300_000
		}),
	}
}

// Interest fades over days: no failure, just a market that should wind down
// rather than linger as dead infrastructure.
func slowBleed() Scenario {
	return Scenario{
		Name:        "slow-bleed",
		Description: "healthy launch, interest fades over three days",
		ShouldList:  true,
		Obs: series(72, func(h int, o *Observation) {
			decay := int64(72 - h)
			depth := 1_500_000 * decay / 72
			if depth < 10_000 {
				depth = 10_000
			}
			o.DepthWithin2Pct = depth
			o.UnderwrittenCapital = 250_000 * decay / 72
			o.Age = time.Duration(100+h) * time.Hour
			o.TopHolderShareBps = 2_000
			o.TopPoolShareBps = 6_500
			o.OracleConfidence = 5_000 + 4_000*decay/72
			o.Volume24h = 10_000_000 * decay / 72
			o.SpotTVL = depth * 3
			o.MarketCap = 35_000_000
			o.RealisedVolBps = 18_000
			o.OpenInterest = 120_000 * decay / 72
		}),
	}
}
