// Command calibrate fits health-engine thresholds against labelled market
// history.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/levu-lol/levu/health"
)

func main() {
	in := flag.String("in", "", "CSV of labelled observations")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "usage: calibrate -in history.csv")
		os.Exit(2)
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	scenarios, err := health.LoadCSV(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}

	obsCount := 0
	for _, s := range scenarios {
		obsCount += len(s.Obs)
	}
	fmt.Printf("%d markets, %d observations\n", len(scenarios), obsCount)

	base := health.DefaultConfig()
	current := health.Evaluate(scenarios, base)
	fmt.Printf("\ncurrent config (list above %d): %d correct, %d false positives, %d false negatives\n",
		base.ListAbove, current.TruePositives+current.TrueNegatives,
		current.FalsePositives, current.FalseNegatives)

	grid := []int64{4_000, 4_500, 5_000, 5_500, 6_000, 6_500, 7_000, 7_500}
	best, all, err := health.Calibrate(scenarios, base, grid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nLIST ABOVE\tTIERS (2x/3x/5x)\tCORRECT\tFALSE POS\tFALSE NEG\tSCORE")
	fmt.Fprintln(w, "----------\t----------------\t-------\t---------\t---------\t-----")
	for _, o := range all {
		t := o.Config.Tiers
		marker := ""
		if o.Config.ListAbove == best.Config.ListAbove {
			marker = "  <- best"
		}
		fmt.Fprintf(w, "%d\t%d/%d/%d\t%d\t%d\t%d\t%d%s\n",
			o.Config.ListAbove, t[2].MinScore, t[1].MinScore, t[0].MinScore,
			o.TruePositives+o.TrueNegatives, o.FalsePositives, o.FalseNegatives,
			o.Score(), marker)
	}
	w.Flush()

	fmt.Printf("\nfitted listing threshold: %d (was %d)\n",
		best.Config.ListAbove, base.ListAbove)
	fmt.Println("\nNote: a threshold is only as good as the labels behind it. These are")
	fmt.Println("fitted to whatever `should_list` says in the CSV, so the fit is worth")
	fmt.Println("exactly as much as those labels are.")
}
