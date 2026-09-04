package health

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LoadCSV reads real observation series so the engine can be calibrated against
// history rather than against the synthetic scenarios in scenarios.go.
//
// Those scenarios validate that the engine does the right thing with a shape it
// should recognise. They cannot tell you whether 5500 is the right listing
// threshold for Robinhood Chain — only real market history can, and this is how
// it gets in.
//
// Columns (header row required, order free):
//
//	market            symbol, grouping rows into one series
//	time              RFC3339
//	depth_2pct        executable depth within 2% of mid, quote units
//	underwritten      bonded first-loss capital
//	age_hours         token age in hours
//	top_holder_bps    largest holder's share of supply
//	top_pool_bps      deepest pool's share of liquidity
//	oracle_confidence basis points
//	volume_24h        quote units
//	spot_tvl          quote units
//	market_cap        quote units
//	realised_vol_bps  annualised
//	open_interest     quote units, one side
//	collapsed_at      RFC3339, optional; blank if the market never failed
//	atomic            true when the collapse had no warning
//	should_list       ground truth: what a careful human would have decided
func LoadCSV(r io.Reader) ([]Scenario, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read csv: %w", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("csv has no data rows")
	}

	col := make(map[string]int, len(rows[0]))
	for i, h := range rows[0] {
		col[strings.TrimSpace(strings.ToLower(h))] = i
	}
	need := []string{"market", "time", "depth_2pct", "oracle_confidence", "volume_24h", "spot_tvl"}
	for _, n := range need {
		if _, ok := col[n]; !ok {
			return nil, fmt.Errorf("csv is missing required column %q", n)
		}
	}

	get := func(row []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	num := func(row []string, name string) (int64, error) {
		v := get(row, name)
		if v == "" {
			return 0, nil
		}
		// Tolerate decimals in source data by truncating: these are whole
		// quote units and sub-unit precision changes no decision.
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("column %s: %w", name, err)
		}
		return int64(f), nil
	}

	byMarket := map[string]*Scenario{}
	order := []string{}

	for n, row := range rows[1:] {
		line := n + 2
		market := get(row, "market")
		if market == "" {
			return nil, fmt.Errorf("line %d: empty market", line)
		}
		ts, err := time.Parse(time.RFC3339, get(row, "time"))
		if err != nil {
			return nil, fmt.Errorf("line %d: time: %w", line, err)
		}

		var o Observation
		o.Time = ts
		for _, f := range []struct {
			name string
			dst  *int64
		}{
			{"depth_2pct", &o.DepthWithin2Pct},
			{"underwritten", &o.UnderwrittenCapital},
			{"top_holder_bps", &o.TopHolderShareBps},
			{"top_pool_bps", &o.TopPoolShareBps},
			{"oracle_confidence", &o.OracleConfidence},
			{"volume_24h", &o.Volume24h},
			{"spot_tvl", &o.SpotTVL},
			{"market_cap", &o.MarketCap},
			{"realised_vol_bps", &o.RealisedVolBps},
			{"open_interest", &o.OpenInterest},
		} {
			v, err := num(row, f.name)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			*f.dst = v
		}
		hours, err := num(row, "age_hours")
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		o.Age = time.Duration(hours) * time.Hour

		sc, ok := byMarket[market]
		if !ok {
			sc = &Scenario{Name: market}
			byMarket[market] = sc
			order = append(order, market)

			if v := get(row, "collapsed_at"); v != "" {
				t, err := time.Parse(time.RFC3339, v)
				if err != nil {
					return nil, fmt.Errorf("line %d: collapsed_at: %w", line, err)
				}
				sc.CollapseAt = t
			}
			sc.Atomic = strings.EqualFold(get(row, "atomic"), "true")
			sc.ShouldList = strings.EqualFold(get(row, "should_list"), "true")
			sc.Description = "loaded from observations"
		}
		sc.Obs = append(sc.Obs, o)
	}

	out := make([]Scenario, 0, len(order))
	for _, m := range order {
		sc := byMarket[m]
		// Observations must be chronological: the lifecycle machine measures
		// dwell times between them, so an out-of-order row would silently
		// shorten or lengthen a threshold's hold time.
		sort.SliceStable(sc.Obs, func(i, j int) bool { return sc.Obs[i].Time.Before(sc.Obs[j].Time) })
		out = append(out, *sc)
	}
	return out, nil
}

// Outcome summarises how a configuration performed across a set of scenarios.
type Outcome struct {
	Config Config
	// Correct listings and refusals against the ground truth.
	TruePositives  int
	TrueNegatives  int
	FalsePositives int // listed something that should not have been listed
	FalseNegatives int // refused something that should have been
	// Total exposure still open across all collapses. This is the number that
	// actually costs money, and it is why accuracy alone is the wrong metric:
	// refusing every market scores perfectly on false positives and serves
	// nobody.
	ExposureAtCollapse int64
}

// Score ranks a configuration.
//
// False positives are weighted far above false negatives on purpose. Refusing a
// market that would have been fine costs fees; listing one that rugs costs the
// insurance fund and the traders in it. The exposure term breaks ties between
// configurations that make the same decisions but grant different caps.
func (o Outcome) Score() int64 {
	return int64(o.TruePositives+o.TrueNegatives)*1_000 -
		int64(o.FalsePositives)*10_000 -
		int64(o.FalseNegatives)*1_000 -
		o.ExposureAtCollapse/1_000
}

// Evaluate runs a configuration over a set of scenarios.
func Evaluate(scenarios []Scenario, cfg Config) Outcome {
	out := Outcome{Config: cfg}
	for _, r := range RunAll(scenarios, cfg) {
		var want bool
		for _, s := range scenarios {
			if s.Name == r.Scenario {
				want = s.ShouldList
				break
			}
		}
		switch {
		case want && r.Listed:
			out.TruePositives++
		case !want && !r.Listed:
			out.TrueNegatives++
		case !want && r.Listed:
			out.FalsePositives++
		default:
			out.FalseNegatives++
		}
		out.ExposureAtCollapse += r.ExposureAtCollapse
	}
	return out
}

// Calibrate grid-searches the listing threshold and tier boundaries.
//
// The search is deliberately small and interpretable rather than a general
// optimiser: a threshold nobody can explain is a threshold nobody should
// deploy, and every candidate here keeps the invariant that the listing
// threshold equals the lowest tier.
func Calibrate(scenarios []Scenario, base Config, thresholds []int64) (Outcome, []Outcome, error) {
	if len(scenarios) == 0 {
		return Outcome{}, nil, fmt.Errorf("calibrate: no scenarios")
	}
	var all []Outcome
	var best Outcome
	found := false

	for _, low := range thresholds {
		cfg := base
		// The ladder is generated from one number so the search stays
		// interpretable, and it carries the spot rung: a candidate without one
		// would be a candidate that drops a fading market off a cliff.
		cfg.Tiers = []Tier{
			{MinScore: low + 3_000, Leverage: 5},
			{MinScore: low + 1_500, Leverage: 3},
			{MinScore: low, Leverage: 2},
			{MinScore: low - 2_000, Leverage: 1},
		}
		cfg.ListAbove = low
		cfg.DegradeBelow = low - 500
		cfg.SpotOnlyBelow = low - 1_000
		cfg.DelistBelow = low - 1_500
		if cfg.Tiers[len(cfg.Tiers)-1].MinScore < 0 {
			continue
		}
		if cfg.DelistBelow < 0 || cfg.DegradeBelow <= cfg.DelistBelow {
			continue
		}
		if err := cfg.Validate(); err != nil {
			continue
		}
		o := Evaluate(scenarios, cfg)
		all = append(all, o)
		if !found || o.Score() > best.Score() {
			best, found = o, true
		}
	}
	if !found {
		return Outcome{}, nil, fmt.Errorf("calibrate: no candidate configuration was valid")
	}
	return best, all, nil
}
