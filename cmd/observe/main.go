// Command observe indexes pools and writes the CSV the health calibrator reads.
//
// This is the last link in the calibration chain: chain state -> observations
// -> CSV -> Calibrate. With an RPC endpoint carrying real market history, the
// engine's thresholds stop being judgment and start being fitted.
//
// Fields a pool cannot answer — holder concentration, bonded underwriting,
// oracle confidence — are left blank rather than guessed, and the health engine
// gates on several of them, so an unsupplied field reads as a refusal. That is
// the safe direction for a decision about granting leverage.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/levu-lol/levu/indexer"
)

func main() {
	var (
		rpc      = flag.String("rpc", "http://127.0.0.1:8545", "RPC endpoint")
		pairs    = flag.String("pairs", "", "comma-separated symbol=pair[:base] entries")
		samples  = flag.Int("samples", 1, "observations per pool")
		interval = flag.Duration("interval", 0, "wait between observations")
		out      = flag.String("out", "-", "output CSV path, or - for stdout")
		blockSec = flag.Int64("block-seconds", 12, "seconds per block, for age and volume windows")
	)
	flag.Parse()

	if strings.TrimSpace(*pairs) == "" {
		fmt.Fprintln(os.Stderr, "no pools given: -pairs SYMBOL=0xpair[:0xbase],...")
		os.Exit(2)
	}
	pools, err := parsePools(*pairs, *blockSec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	obs := indexer.New(&indexer.RPCChain{RPC: *rpc})
	ctx := context.Background()

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()

	header := []string{
		"market", "time", "depth_2pct", "underwritten", "age_hours",
		"top_holder_bps", "top_pool_bps", "oracle_confidence",
		"volume_24h", "spot_tvl", "market_cap", "realised_vol_bps", "open_interest",
	}
	if err := cw.Write(header); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}

	for i := 0; i < *samples; i++ {
		if i > 0 && *interval > 0 {
			time.Sleep(*interval)
		}
		now := time.Now().UTC()
		for _, p := range pools {
			o, err := obs.Observe(ctx, p, now)
			if err != nil {
				fmt.Fprintf(os.Stderr, "observe %s: %v\n", p.Symbol, err)
				continue
			}
			row := []string{
				p.Symbol,
				now.Format(time.RFC3339),
				fmt.Sprint(o.DepthWithin2Pct),
				"", // underwritten: not on chain
				fmt.Sprint(int64(o.Age.Hours())),
				"", // top_holder_bps: needs a transfer index
				fmt.Sprint(o.TopPoolShareBps),
				"", // oracle_confidence: from the aggregator, not the pool
				fmt.Sprint(o.Volume24h),
				fmt.Sprint(o.SpotTVL),
				fmt.Sprint(o.MarketCap),
				"", // realised vol: needs a price series
				fmt.Sprint(o.OpenInterest),
			}
			if err := cw.Write(row); err != nil {
				fmt.Fprintln(os.Stderr, "write:", err)
				os.Exit(1)
			}
		}
		cw.Flush()
	}
}

// parsePools reads "SYMBOL=0xpair[:0xbase]" entries.
func parsePools(spec string, blockSeconds int64) ([]indexer.Pool, error) {
	var pools []indexer.Pool
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rest, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("malformed pool %q: want SYMBOL=0xpair", entry)
		}
		pair, base, _ := strings.Cut(rest, ":")
		pools = append(pools, indexer.Pool{
			Symbol:       strings.TrimSpace(name),
			Pair:         strings.TrimSpace(pair),
			BaseToken:    strings.TrimSpace(base),
			BlockSeconds: blockSeconds,
		})
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("no pools parsed")
	}
	return pools, nil
}
