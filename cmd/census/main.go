// Command census answers one question against a real chain: of every token
// here, how many could we actually list, and at what leverage?
//
// It does not reimplement the gates. It loads observations measured from chain
// and runs the same health.Assess and health.Derive the exchange runs, so the
// answer moves when the engine moves. What it adds is the funnel: which
// condition eliminated how many, which is the part you cannot see from a single
// verdict.
//
// Inputs it cannot measure from a public RPC are named as assumptions rather
// than quietly defaulted. Supply concentration needs a holder index; the
// capital we would underwrite a lane with is our decision, not a fact about
// the chain.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/levu-lol/levu/health"
)

type row struct {
	Symbol    string  `json:"symbol"`
	Quote     string  `json:"quote"`
	Pool      string  `json:"pool"`
	Pools     int     `json:"pools"`
	TVL       float64 `json:"tvl_usd"`
	Depth2Pct float64 `json:"depth_2pct_usd"`
	Volume24h float64 `json:"volume_24h_usd"`
	AgeHours  float64 `json:"age_hours"`
	TopPool   int64   `json:"top_pool_share_bps"`
	VolBps    int64   `json:"realised_vol_bps"`
	MarketCap float64 `json:"market_cap_usd"`
	Conf      int64   `json:"oracle_confidence_bps"`
	Swaps     int     `json:"swaps_24h"`
	Dec0      int     `json:"dec0"`
	Dec1      int     `json:"dec1"`
	QuoteIs0  bool    `json:"quote_is_token0"`
	Fee       int     `json:"fee"`
	Price     float64 `json:"price"`
	Base      string  `json:"base"`
	BaseDec   int     `json:"base_dec"`
}

// emitted is one market as the front end needs it: enough to read its price
// from chain, plus what the engine decided about it.
type emitted struct {
	row
	MaxLeverage int64    `json:"max_leverage"`
	OICap       int64    `json:"oi_cap"`
	MaxPosition int64    `json:"max_position"`
	Score       int64    `json:"score"`
	Maintenance int64    `json:"maintenance_bps"`
	SpotOnly    bool     `json:"spot_only"`
	Blockers    []string `json:"leverage_blockers,omitempty"`
}

func main() {
	var (
		in     = flag.String("in", "census.json", "measured observations")
		holder = flag.Int64("holder-bps", 0, "assumed top-holder share; 0 leaves it unknown, which the engine scores as the worst case")
		under  = flag.Int64("underwriting", 0, "capital we would bond behind each lane, in quote units")
		book   = flag.Int64("book", 0, "resting depth we would quote in our own lane, in quote units")
		full   = flag.Bool("all", false, "list every survivor, not just the top 30")
		emit   = flag.String("emit", "", "write the listable markets to this file, for the front end")
	)
	flag.Parse()

	b, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var rows []row
	if err := json.Unmarshal(b, &rows); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg := health.DefaultConfig()
	now := time.Now()

	type result struct {
		row
		score health.Score
		cap   health.Capacity
		ok    bool
	}
	var res []result
	gateCount := map[string]int{}
	blockCount := map[string]int{}

	for _, r := range rows {
		o := health.Observation{
			Time:                now,
			DepthWithin2Pct:     int64(r.Depth2Pct),
			UnderwrittenCapital: *under,
			Age:                 time.Duration(r.AgeHours * float64(time.Hour)),
			TopHolderShareBps:   *holder,
			TopPoolShareBps:     r.TopPool,
			OracleConfidence:    r.Conf,
			Volume24h:           int64(r.Volume24h),
			SpotTVL:             int64(r.TVL),
			MarketCap:           int64(r.MarketCap),
			BookDepthWithin2Pct: *book,
			RealisedVolBps:      r.VolBps,
		}
		s := health.Assess(o, cfg)
		c, ok := health.Derive(o, s, cfg)
		res = append(res, result{r, s, c, ok})
		for _, g := range s.GateFailures {
			gateCount[g]++
		}
		if len(s.GateFailures) == 0 {
			for _, g := range s.LeverageBlockers {
				blockCount[g]++
			}
		}
	}

	fmt.Printf("\nassumptions: top-holder=%s  underwriting=$%s  our book=$%s\n\n",
		bpsOrUnknown(*holder), comma(*under), comma(*book))

	fmt.Printf("tokens measured                     %6d\n", len(res))

	// The funnel, in the order the engine applies it.
	eligible, listed, levered := 0, 0, 0
	for _, r := range res {
		if r.score.Eligible() {
			eligible++
		}
		if r.ok {
			listed++
			if r.cap.MaxLeverage > 1 {
				levered++
			}
		}
	}
	fmt.Printf("  passed every gate                 %6d\n", eligible)
	fmt.Printf("  and scored above the 1x rung      %6d   <- listable on spot\n", listed)
	fmt.Printf("  of those, leverable above 1x      %6d\n\n", levered)

	fmt.Println("what eliminated the rest (a token can fail more than one):")
	printCounts(gateCount, "  gate     ")
	fmt.Println("\nof the tokens that passed every gate, what withdrew leverage:")
	printCounts(blockCount, "  blocker  ")

	sort.Slice(res, func(i, j int) bool { return res[i].score.Total > res[j].score.Total })

	fmt.Println("\nlistable markets:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  SYMBOL\tQUOTE\tTVL\tDEPTH2%\tVOL24H\tAGE\tSCORE\tLEV\tOI CAP")
	n := 0
	for _, r := range res {
		if !r.ok {
			continue
		}
		n++
		if !*full && n > 30 {
			fmt.Fprintf(w, "  ... and %d more\t\t\t\t\t\t\t\t\n", listed-30)
			break
		}
		fmt.Fprintf(w, "  %s\t%s\t$%s\t$%s\t$%s\t%dd\t%d\t%dx\t$%s\n",
			r.Symbol, r.Quote, comma(int64(r.TVL)), comma(int64(r.Depth2Pct)),
			comma(int64(r.Volume24h)), int(r.AgeHours/24),
			r.score.Total, r.cap.MaxLeverage, comma(r.cap.OICap))
	}
	w.Flush()

	if *emit != "" {
		out := make([]emitted, 0, listed)
		for _, r := range res {
			if !r.ok {
				continue
			}
			out = append(out, emitted{
				row: r.row, MaxLeverage: r.cap.MaxLeverage, OICap: r.cap.OICap,
				MaxPosition: r.cap.MaxPosition, Score: r.score.Total,
				Maintenance: r.cap.MaintenanceBps,
				SpotOnly:    r.cap.MaxLeverage <= 1, Blockers: r.score.LeverageBlockers,
			})
		}
		b, err := json.MarshalIndent(out, "", " ")
		if err == nil {
			err = os.WriteFile(*emit, b, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("\nwrote %d markets to %s\n", len(out), *emit)
	}
}

func printCounts(m map[string]int, prefix string) {
	type kv struct {
		k string
		n int
	}
	var xs []kv
	for k, n := range m {
		xs = append(xs, kv{k, n})
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].n > xs[j].n })
	if len(xs) == 0 {
		fmt.Println(prefix + "(none)")
		return
	}
	for _, x := range xs {
		fmt.Printf("%s%-52s %6d\n", prefix, x.k, x.n)
	}
}

func bpsOrUnknown(b int64) string {
	if b <= 0 {
		return "unknown (scored worst case)"
	}
	return fmt.Sprintf("%d bps", b)
}

func comma(v int64) string {
	s := fmt.Sprintf("%d", v)
	if v < 0 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
