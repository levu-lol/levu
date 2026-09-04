// Produces the real engine outputs behind the CASHCAT market view.
package main

import (
	"fmt"
	"time"

	"github.com/levu-lol/levu/health"
)

func report(name string, o health.Observation, cfg health.Config) {
	s := health.Assess(o, cfg)
	c, ok := health.Derive(o, s, cfg)
	fmt.Printf("\n=== %s ===\n", name)
	fmt.Printf("score            %d\n", s.Total)
	fmt.Printf("  depth          %d\n", s.Sub.Depth)
	fmt.Printf("  underwriting   %d\n", s.Sub.Underwriting)
	fmt.Printf("  oracle         %d\n", s.Sub.Oracle)
	fmt.Printf("  maturity       %d\n", s.Sub.Maturity)
	fmt.Printf("  dispersion     %d\n", s.Sub.Dispersion)
	fmt.Printf("  stability      %d\n", s.Sub.Stability)
	fmt.Printf("eligible         %v %v\n", s.Eligible(), s.GateFailures)
	if ok {
		fmt.Printf("leverage         %dx\n", c.MaxLeverage)
		fmt.Printf("oi cap           %d\n", c.OICap)
		fmt.Printf("initial          %d bps\n", c.InitialBps)
		fmt.Printf("maintenance      %d bps\n", c.MaintenanceBps)
	} else {
		fmt.Printf("no market\n")
	}
}

func main() {
	cfg := health.DefaultConfig()

	usdg := health.Observation{
		Time:                time.Now(),
		DepthWithin2Pct:     840_000,
		UnderwrittenCapital: 260_000,
		Age:                 19 * 24 * time.Hour,
		TopHolderShareBps:   1_450,
		TopPoolShareBps:     5_800,
		OracleConfidence:    8_100,
		Volume24h:           14_200_000,
		SpotTVL:             3_100_000,
		MarketCap:           47_000_000,
		RealisedVolBps:      24_500,
		OpenInterest:        512_000,
	}
	eth := usdg
	eth.DepthWithin2Pct = 615_000
	eth.UnderwrittenCapital = 155_000
	eth.OracleConfidence = 6_900
	eth.TopPoolShareBps = 7_400
	eth.Volume24h = 3_900_000
	eth.SpotTVL = 1_150_000
	eth.OpenInterest = 402_000

	report("CASHCAT/USDG", usdg, cfg)
	report("CASHCAT/ETH", eth, cfg)

	// Utilisation and rent, from the same curve the VM uses.
	printRent := func(name string, oi int64, o health.Observation) {
		c, _ := health.Derive(o, health.Assess(o, cfg), cfg)
		if c.OICap == 0 {
			fmt.Printf("\n%s: no capacity\n", name)
			return
		}
		u := float64(oi) / float64(c.OICap)
		// Mirrors capacity::RentCurve::conservative_default in the VM.
		base, kink, below, above := 0.00001, 0.8, 0.00005, 0.002
		var rate float64
		if u <= kink {
			rate = base + below*(u/kink)
		} else {
			rate = base + below + above*((u-kink)/(1-kink))
		}
		fmt.Printf("\n%s\n  utilisation  %.1f%% (%d / %d)\n  rent/interval %.6f%%\n  rent annualised ~%.2f%%\n",
			name, u*100, oi, c.OICap, rate*100, rate*24*365*100)
	}
	printRent("CASHCAT/USDG", usdg.OpenInterest, usdg)
	printRent("CASHCAT/ETH", eth.OpenInterest, eth)
	if false {
	}
}
