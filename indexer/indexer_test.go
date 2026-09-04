package indexer_test

import (
	"context"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/levu-lol/levu/health"
	"github.com/levu-lol/levu/indexer"
)

const (
	anvilKey  = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilPort = "8547"
	anvilRPC  = "http://127.0.0.1:8547"
)

func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"anvil", "forge", "cast"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

func startAnvil(t *testing.T) {
	t.Helper()
	cmd := exec.Command("anvil", "--silent", "--port", anvilPort)
	if err := cmd.Start(); err != nil {
		t.Skipf("anvil: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("cast", "block-number", "--rpc-url", anvilRPC).Run(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Skip("anvil did not become ready")
}

func contractsDir(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", "contracts"))
}

func deploy(t *testing.T, contract string, args ...string) string {
	t.Helper()
	full := append([]string{"create", "--rpc-url", anvilRPC, "--private-key", anvilKey,
		"--broadcast", contract}, args...)
	cmd := exec.Command("forge", full...)
	cmd.Dir = contractsDir(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deploy %s: %v\n%s", contract, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Deployed to:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Deployed to:"))
		}
	}
	t.Fatalf("no address for %s:\n%s", contract, out)
	return ""
}

func send(t *testing.T, to, sig string, args ...string) {
	t.Helper()
	full := append([]string{"send", to, sig}, args...)
	full = append(full, "--private-key", anvilKey, "--rpc-url", anvilRPC)
	if out, err := exec.Command("cast", full...).CombinedOutput(); err != nil {
		t.Fatalf("send %s: %v\n%s", sig, err, out)
	}
}

func wad(n int64) string {
	v := new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	return v.String()
}

// A pool with a million quote units on each side.
func livePool(t *testing.T) (indexer.Pool, *indexer.Observer) {
	t.Helper()
	base := deploy(t, "test/mocks/MockPair.sol:MockToken", "--constructor-args", "MEME", wad(1_000_000_000))
	quote := deploy(t, "test/mocks/MockPair.sol:MockToken", "--constructor-args", "USDG", wad(1_000_000_000))
	pair := deploy(t, "test/mocks/MockPair.sol:MockPair",
		"--constructor-args", base, quote, wad(10_000_000), wad(1_000_000))

	head, _ := exec.Command("cast", "block-number", "--rpc-url", anvilRPC).Output()
	created, _ := strconv.ParseInt(strings.TrimSpace(string(head)), 10, 64)

	return indexer.Pool{
			Symbol: "MEME", Pair: pair, BaseToken: base,
			QuoteIsToken0: false, CreatedBlock: created, BlockSeconds: 12,
		},
		indexer.New(&indexer.CastChain{RPC: anvilRPC})
}

func TestReservesAreReadInBaseQuoteOrder(t *testing.T) {
	requireTools(t)
	startAnvil(t)
	p, o := livePool(t)

	base, quote, err := o.Reserves(context.Background(), p)
	if err != nil {
		t.Fatalf("reserves: %v", err)
	}
	if base.String() != wad(10_000_000) {
		t.Errorf("base = %s", base)
	}
	if quote.String() != wad(1_000_000) {
		t.Errorf("quote = %s", quote)
	}
}

// / The number that matters most, and the one a naive indexer gets wrong: a
// / pool holding $1M of quote does not have $1M of executable depth. Moving a
// / constant-product price 2% only takes about 0.985% of the reserve per side.
func TestExecutableDepthIsNotTheWholeReserve(t *testing.T) {
	requireTools(t)
	startAnvil(t)
	p, o := livePool(t)

	obs, err := o.Observe(context.Background(), p, time.Now())
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	// 2 * 1_000_000 * (1 - 1/sqrt(1.02)) ~= 19_704
	want := int64(float64(1_000_000) * (1 - 1/math.Sqrt(1.02)) * 2)
	if diff := obs.DepthWithin2Pct - want; diff > 2 || diff < -2 {
		t.Errorf("depth = %d, want ~%d", obs.DepthWithin2Pct, want)
	}
	if obs.DepthWithin2Pct >= 1_000_000 {
		t.Error("depth must be far below the reserve; a listing rule that confuses " +
			"them grants leverage no book can support")
	}
	if obs.SpotTVL != 2_000_000 {
		t.Errorf("TVL = %d, want 2000000", obs.SpotTVL)
	}
}

func TestMarketCapUsesThePoolPrice(t *testing.T) {
	requireTools(t)
	startAnvil(t)
	p, o := livePool(t)

	obs, err := o.Observe(context.Background(), p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// price = 1_000_000 / 10_000_000 = 0.1; supply 1e9 -> mcap 1e8
	if obs.MarketCap != 100_000_000 {
		t.Errorf("market cap = %d, want 100000000", obs.MarketCap)
	}
}

// Draining a pool must show up as depth collapsing, which is what drives the
// health engine's degradation.
func TestDrainingThePoolCollapsesObservedDepth(t *testing.T) {
	requireTools(t)
	startAnvil(t)
	p, o := livePool(t)
	ctx := context.Background()

	before, err := o.Observe(ctx, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	send(t, p.Pair, "setReserves(uint112,uint112)", wad(10_000_000), wad(20_000))
	after, err := o.Observe(ctx, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if after.DepthWithin2Pct >= before.DepthWithin2Pct/10 {
		t.Errorf("depth barely moved after a 98%% drain: %d -> %d",
			before.DepthWithin2Pct, after.DepthWithin2Pct)
	}
}

func TestSwapVolumeIsIndexedFromLogs(t *testing.T) {
	requireTools(t)
	startAnvil(t)
	p, o := livePool(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		send(t, p.Pair, "recordSwap(uint256,uint256,uint256,uint256)",
			"0", wad(100), "0", wad(50))
	}
	obs, err := o.Observe(ctx, p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if obs.Volume24h != 150 {
		t.Errorf("volume = %d, want 150 (three swaps of 50 quote out)", obs.Volume24h)
	}
}

// Observations feed the engine directly, so a real pool must produce a real
// decision rather than something the engine chokes on.
func TestObservationsDriveTheHealthEngine(t *testing.T) {
	requireTools(t)
	startAnvil(t)
	p, o := livePool(t)

	obs, err := o.Observe(context.Background(), p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Fields a pool cannot answer must be supplied by whoever has them.
	obs.OracleConfidence = 9_000
	obs.UnderwrittenCapital = 200_000
	obs.TopHolderShareBps = 1_000
	obs.RealisedVolBps = 15_000

	s := health.Assess(obs, health.DefaultConfig())
	if s.Total <= 0 {
		t.Error("a live pool produced a zero score")
	}
	// This pool has ~$20k of executable depth against a $50k floor, so it is
	// correctly refused: the market cap is large and the book is not.
	if s.Eligible() {
		t.Errorf("a pool with %d executable depth should not be listable",
			obs.DepthWithin2Pct)
	}
}
