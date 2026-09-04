// Command oraclefeed runs the real aggregator over real pools.
//
// Each Uniswap fee tier is a separate pool with its own depth, so each is its
// own price source — which is exactly the independence the aggregator's
// coverage term counts. Read-only: pool state and token metadata, nothing
// signed or sent.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/levu-lol/levu/indexer"
	"github.com/levu-lol/levu/oracle"
)

const (
	defaultRPC = "https://rpc.mainnet.chain.robinhood.com"
	factory    = "0x1f7d7550b1b028f7571e69a784071f0205fd2efa"
	weth       = "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73"
	usdg       = "0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168"
)

func main() {
	var (
		rpc     = flag.String("rpc", defaultRPC, "RPC endpoint")
		base    = flag.String("base", weth, "base token")
		quote   = flag.String("quote", usdg, "quote token")
		n       = flag.Int("n", 5, "readings to take, 0 for continuous")
		every   = flag.Duration("every", 6*time.Second, "interval between readings")
		haircut = flag.Int64("haircut", 3_000, "depth haircut in bps")
	)
	flag.Parse()

	chain := &indexer.CastChain{RPC: *rpc}
	obs := indexer.New(chain)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dec := func(token string) uint8 {
		out, err := chain.Call(ctx, token, "decimals()(uint8)")
		if err != nil || len(out) == 0 {
			return 18
		}
		var d uint8
		fmt.Sscan(out[0], &d)
		return d
	}

	venues, err := indexer.PoolVenues(ctx, obs, chain, factory, *base, *quote,
		dec(*base), dec(*quote), *haircut)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover pools:", err)
		os.Exit(1)
	}
	names := make([]string, 0, len(venues))
	for _, v := range venues {
		names = append(names, v.Name())
	}
	fmt.Printf("%d venues: %s\n", len(venues), strings.Join(names, ", "))

	cfg := oracle.DefaultFeedConfig()
	cfg.Interval = *every
	cfg.VenueTimeout = 4 * time.Second
	feed := oracle.NewFeed(venues, cfg)

	fmt.Printf("\n%-12s %-16s %-6s %-9s %-9s %-9s %s\n",
		"TIME", "INDEX", "CONF", "COVERAGE", "DEPTH", "AGREE", "USED")
	fmt.Println(strings.Repeat("-", 92))

	for i := 0; *n == 0 || i < *n; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(*every):
			}
		}
		r := feed.Read(ctx, time.Now().UTC())
		res := r.Result

		status := "ok"
		if !r.Usable() {
			status = "UNUSABLE"
		}
		fmt.Printf("%-12s %-16s %-6d %-9s %-9s %-9s %s  %s\n",
			r.At.Format("15:04:05"),
			trim(res.Price.String()),
			res.Confidence,
			pct(res.Coverage), pct(res.Depth), pct(res.Agreement),
			strings.Join(res.Used, " "),
			status)

		for _, f := range r.Failed {
			fmt.Printf("%-12s   venue %s unavailable: %s\n", "", f.Venue, oneline(f.Err))
		}
		for _, rj := range res.Reject {
			if rj.Name != "" {
				fmt.Printf("%-12s   %s excluded: %s\n", "", rj.Name, rj.Reason)
			}
		}
	}
}

func pct(f interface{ String() string }) string {
	s := f.String()
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s) > i+3 {
		s = s[:i+3]
	}
	return s
}

func trim(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s) > i+5 {
		return s[:i+5]
	}
	return s
}

func oneline(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
