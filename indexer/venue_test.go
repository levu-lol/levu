package indexer_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/levu-lol/levu/indexer"
	"github.com/levu-lol/levu/oracle"
	"github.com/levu-lol/levu/wire"
)

// Robinhood Chain mainnet. Read-only: pool state and token metadata.
const (
	rhRPC     = "https://rpc.mainnet.chain.robinhood.com"
	rhFactory = "0x1f7d7550b1b028f7571e69a784071f0205fd2efa"
	rhWETH    = "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73"
	rhUSDG    = "0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168"
)

// Live-chain tests are opt-in: they depend on a public endpoint being reachable
// and rate-limited, which is not a property a test suite should assert.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("LIVE_CHAIN") == "" {
		t.Skip("set LIVE_CHAIN=1 to run against Robinhood Chain mainnet")
	}
	requireTools(t)
}

func liveVenues(t *testing.T) []oracle.Venue {
	t.Helper()
	chain := &indexer.CastChain{RPC: rhRPC}
	obs := indexer.New(chain)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v, err := indexer.PoolVenues(ctx, obs, chain, rhFactory, rhWETH, rhUSDG, 18, 6, 3_000)
	if err != nil {
		t.Fatalf("discover pools: %v", err)
	}
	return v
}

func TestLivePoolsQuoteAPlausiblePrice(t *testing.T) {
	requireLive(t)
	venues := liveVenues(t)
	if len(venues) < 2 {
		t.Fatalf("found %d venues, expected several fee tiers", len(venues))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	feed := oracle.NewFeed(venues, oracle.DefaultFeedConfig())
	r := feed.Read(ctx, time.Now().UTC())
	if !r.Usable() {
		t.Fatalf("live reading unusable: failures %+v, rejects %+v",
			r.Failed, r.Result.Reject)
	}

	// No fixed expectation: this is a live price. What is asserted is that the
	// venues aggregate at all and agree — a wrong sqrtPriceX96 or decimal
	// conversion misses by orders of magnitude, not by percent, so it would
	// show up as venues disagreeing rather than as a slightly odd number.
	t.Logf("live index %s, confidence %d bps, used %v",
		r.Result.Price, r.Result.Confidence, r.Result.Used)

	if r.Result.Confidence == 0 {
		t.Error("live venues produced zero confidence")
	}
	if len(r.Result.Used) < 2 {
		t.Errorf("only %d venues survived aggregation: %v", len(r.Result.Used), r.Result.Used)
	}
}

// Independent pools of the same pair must agree closely; wide disagreement
// would mean the price conversion is wrong rather than that the market is.
func TestLivePoolsAgreeWithEachOther(t *testing.T) {
	requireLive(t)
	venues := liveVenues(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	feed := oracle.NewFeed(venues, oracle.DefaultFeedConfig())
	r := feed.Read(ctx, time.Now().UTC())
	if !r.Usable() {
		t.Skipf("no usable reading: %+v", r.Failed)
	}
	// Agreement is the weighted mean deviation as a fraction of the deviation
	// budget; anything near 1 means the pools are quoting the same price.
	if r.Result.Agreement.Cmp(wire.FixedRawInt64(500_000_000_000_000_000)) < 0 {
		t.Errorf("pools disagree more than expected: agreement %s", r.Result.Agreement)
	}
}
