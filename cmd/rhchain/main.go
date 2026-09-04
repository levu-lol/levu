// Command rhchain reads live Robinhood Chain pools and reports what the health
// engine makes of them.
//
// Read-only: pool state, token metadata, balances. Nothing is signed or sent.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/levu-lol/levu/health"
	"github.com/levu-lol/levu/indexer"
)

const (
	defaultRPC = "https://rpc.mainnet.chain.robinhood.com"
	factory    = "0x1f7d7550b1b028f7571e69a784071f0205fd2efa"
	weth       = "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73"
	usdg       = "0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168"
)

func main() {
	rpc := flag.String("rpc", defaultRPC, "Robinhood Chain RPC")
	base := flag.String("base", weth, "base token address")
	quote := flag.String("quote", usdg, "quote token address")
	label := flag.String("label", "WETH/USDG", "symbol for the report")
	csvOut := flag.String("csv", "", "also append an observation row to this CSV")
	flag.Parse()

	chain := &indexer.RPCChain{RPC: *rpc}
	obs := indexer.New(chain)
	ctx := context.Background()

	dec := func(token string) uint8 {
		out, err := chain.Call(ctx, token, "decimals()(uint8)")
		if err != nil || len(out) == 0 {
			return 18
		}
		var d uint8
		fmt.Sscan(out[0], &d)
		return d
	}
	baseDec, quoteDec := dec(*base), dec(*quote)

	supply := new(big.Int)
	if out, err := chain.Call(ctx, *base, "totalSupply()(uint256)"); err == nil && len(out) > 0 {
		supply.SetString(out[0], 10)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nFEE\tPOOL\tPRICE\tDEPTH ±2%\tTVL\tSCORE\tDECISION")
	fmt.Fprintln(w, "---\t----\t-----\t---------\t---\t-----\t--------")

	cfg := health.DefaultConfig()
	var best health.Observation
	var bestPool string

	for _, fee := range []string{"100", "500", "3000", "10000"} {
		out, err := chain.Call(ctx, factory, "getPool(address,address,uint24)(address)",
			*base, *quote, fee)
		if err != nil || len(out) == 0 {
			continue
		}
		pool := out[0]
		if strings.EqualFold(pool, "0x0000000000000000000000000000000000000000") {
			continue
		}

		// token0 is whichever address sorts lower, which decides orientation.
		t0, err := chain.Call(ctx, pool, "token0()(address)")
		if err != nil || len(t0) == 0 {
			continue
		}
		quoteIsToken0 := strings.EqualFold(t0[0], *quote)

		p := indexer.V3Pool{
			Symbol: *label, Pool: pool,
			Token0: t0[0], Token1: *quote,
			QuoteIsToken0: quoteIsToken0,
			BaseSupply:    supply,
			// Liquidity is assumed flat across the band, which over-estimates
			// depth once a tick boundary is crossed. Haircut keeps the error
			// pointing the safe way.
			DepthHaircutBps: 3_000,
		}
		if quoteIsToken0 {
			p.Token1 = *base
			p.Decimals0, p.Decimals1 = quoteDec, baseDec
		} else {
			p.Token0, p.Token1 = *base, *quote
			p.Decimals0, p.Decimals1 = baseDec, quoteDec
		}

		o, state, err := obs.ObserveV3(ctx, p, time.Now().UTC())
		if err != nil {
			fmt.Fprintf(os.Stderr, "fee %s: %v\n", fee, err)
			continue
		}
		// Fields a pool cannot answer, supplied conservatively so the score is
		// about liquidity rather than about assumptions.
		o.Age = 60 * 24 * time.Hour
		o.OracleConfidence = 7_500
		o.TopHolderShareBps = 1_000
		o.RealisedVolBps = 8_000
		o.Volume24h = o.DepthWithin2Pct * 10
		o.UnderwrittenCapital = 0

		s := health.Assess(o, cfg)
		decision := "no market"
		if c, ok := health.Derive(o, s, cfg); ok {
			decision = fmt.Sprintf("%dx, cap %s", c.MaxLeverage, commas(c.OICap))
		} else if !s.Eligible() {
			decision = "gated: " + s.GateFailures[0]
		}

		price := new(big.Float).Quo(
			new(big.Float).SetInt(p.Price(state)),
			new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))

		fmt.Fprintf(w, "%s\t%s\t%.2f\t%s\t%s\t%d\t%s\n",
			fee, pool[:10]+"…", price,
			commas(o.DepthWithin2Pct), commas(o.SpotTVL), s.Total, decision)

		if o.DepthWithin2Pct > best.DepthWithin2Pct {
			best, bestPool = o, pool
		}
		// Each fee tier is a distinct market with its own liquidity profile, so
		// each is recorded separately rather than collapsed into one row.
		if *csvOut != "" {
			label := fmt.Sprintf("%s-%s", *label, fee)
			shouldList := o.DepthWithin2Pct >= 1_000_000
			if err := appendCSV(*csvOut, label, o, shouldList); err != nil {
				fmt.Fprintln(os.Stderr, "csv:", err)
			}
		}
	}
	w.Flush()

	if bestPool == "" {
		fmt.Println("\nno pools found")
		return
	}
	fmt.Printf("\ndeepest pool %s: $%s executable within 2%%, $%s TVL\n",
		bestPool[:10]+"…", commas(best.DepthWithin2Pct), commas(best.SpotTVL))

	if *csvOut != "" {
		fmt.Printf("appended observations to %s\n", *csvOut)
	}
}

// shouldList is the ground-truth label. Here it is a stand-in — a pool with
// over a million dollars executable within 2% is one a careful human would
// list — and real calibration needs labels drawn from what actually happened
// to these markets, not from a rule of thumb about their depth.
func appendCSV(path, symbol string, o health.Observation, shouldList bool) error {
	newFile := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		newFile = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if newFile {
		fmt.Fprintln(f, "market,time,depth_2pct,underwritten,age_hours,top_holder_bps,"+
			"top_pool_bps,oracle_confidence,volume_24h,spot_tvl,market_cap,"+
			"realised_vol_bps,open_interest,should_list")
	}
	_, err = fmt.Fprintf(f, "%s,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%v\n",
		symbol, o.Time.Format(time.RFC3339), o.DepthWithin2Pct, o.UnderwrittenCapital,
		int64(o.Age.Hours()), o.TopHolderShareBps, o.TopPoolShareBps,
		o.OracleConfidence, o.Volume24h, o.SpotTVL, o.MarketCap,
		o.RealisedVolBps, o.OpenInterest, shouldList)
	return err
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
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
