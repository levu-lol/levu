package main

import (
	"fmt"
	"github.com/levu-lol/levu/health"
)

func main() {
	cfg := health.DefaultConfig()
	fmt.Printf("%-15s %6s %6s %6s %6s %6s %6s %6s  %s\n",
		"scenario", "TOTAL", "depth", "undwr", "oracle", "matur", "disp", "stab", "gates")
	for _, s := range health.Scenarios() {
		o := s.Obs[len(s.Obs)/3] // a representative mid-life observation
		sc := health.Assess(o, cfg)
		fmt.Printf("%-15s %6d %6d %6d %6d %6d %6d %6d  %v\n",
			s.Name, sc.Total, sc.Sub.Depth, sc.Sub.Underwriting, sc.Sub.Oracle,
			sc.Sub.Maturity, sc.Sub.Dispersion, sc.Sub.Stability, sc.GateFailures)
	}
	fmt.Printf("\nListAbove=%d  lowest tier=%d\n", cfg.ListAbove, cfg.Tiers[len(cfg.Tiers)-1].MinScore)
}
