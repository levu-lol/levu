// Command healthreport runs the Market Health Engine over the scenario suite
// and prints what it would have decided.
package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/levu-lol/levu/health"
)

func main() {
	cfg := health.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config invalid:", err)
		os.Exit(1)
	}

	scenarios := health.Scenarios()
	results := health.RunAll(scenarios, cfg)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nSCENARIO\tSHOULD\tLISTED\tLEV\tOI CAP\tFINAL\tVERDICT")
	fmt.Fprintln(w, "--------\t------\t------\t---\t------\t-----\t-------")
	misses := 0
	for i, r := range results {
		s := scenarios[i]
		should := "no"
		if s.ShouldList {
			should = "yes"
		}
		listed := "no"
		if r.Listed {
			listed = "yes"
		}
		if len(r.Verdict) > 5 && r.Verdict[:4] == "MISS" {
			misses++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%dx\t%d\t%s\t%s\n",
			r.Scenario, should, listed, r.PeakLeverage, r.PeakOICap, r.Final, r.Verdict)
	}
	w.Flush()

	fmt.Println("\nTRANSITIONS")
	for i, r := range results {
		if len(r.Transitions) == 0 {
			continue
		}
		fmt.Printf("\n  %s — %s\n", r.Scenario, scenarios[i].Description)
		for _, t := range r.Transitions {
			fmt.Printf("    %s  %-11s -> %-11s score %5d  lev %dx  cap %-9d  %s\n",
				t.At.Format("Jan 02 15:04"), t.From, t.To, t.Score,
				t.Capacity.MaxLeverage, t.Capacity.OICap, t.Reason)
		}
	}

	fmt.Printf("\n%d scenarios, %d disagreements with the expected decision\n",
		len(results), misses)
	if misses > 0 {
		os.Exit(1)
	}
}
