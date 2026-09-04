package indexer

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const liveRPC = "https://rpc.mainnet.chain.robinhood.com"

// The native client and cast must agree against the same chain.
//
// This is the check that made replacing one with the other safe: every price
// this system reads goes through Chain.Call, and a decoding difference would
// move marks silently rather than fail.
func TestRPCChainAgreesWithCast(t *testing.T) {
	if _, err := exec.LookPath("cast"); err != nil {
		t.Skipf("cast not installed, nothing to compare against: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	native := &RPCChain{RPC: liveRPC}
	shell := &CastChain{RPC: liveRPC}

	const pool = "0x52e65b17fb6e5ba00ed806f37afcd2daa50271ca" // WETH/USDG
	const usdg = "0x5fc5360D0400a0Fd4f2af552ADD042D716F1d168"

	cases := []struct {
		name, to, sig string
		args          []string
		// stable is true when the value does not move between two calls.
		stable bool
	}{
		{"slot0", pool, "slot0()(uint160,int24,uint16,uint16,uint16,uint8,bool)", nil, false},
		{"liquidity", pool, "liquidity()(uint128)", nil, false},
		{"token0", pool, "token0()(address)", nil, true},
		{"totalSupply", usdg, "totalSupply()(uint256)", nil, true},
		{"balanceOf", usdg, "balanceOf(address)(uint256)", []string{pool}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := native.Call(ctx, tc.to, tc.sig, tc.args...)
			if err != nil {
				t.Fatalf("native: %v", err)
			}
			b, err := shell.Call(ctx, tc.to, tc.sig, tc.args...)
			if err != nil {
				t.Fatalf("cast: %v", err)
			}
			if len(a) == 0 || len(a) != len(b) {
				t.Fatalf("native returned %d words, cast %d", len(a), len(b))
			}
			for i := range a {
				switch {
				case a[i] == "":
					t.Fatalf("word %d empty", i)
				case !tc.stable:
					// Prices, liquidity and balances move between the two
					// calls; what is checked is that both decode to something.
				case strings.EqualFold(a[i], b[i]):
					// cast checksums addresses (EIP-55), we lowercase them.
					// Both name the same address and every comparison in this
					// package is case-insensitive.
				default:
					t.Fatalf("word %d: native %q, cast %q", i, a[i], b[i])
				}
			}
		})
	}
}

func TestRPCChainReadsTheHead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := (&RPCChain{RPC: liveRPC}).BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 50_000_000 {
		t.Fatalf("head %d looks wrong for this chain", n)
	}
}

// A signature the table does not know must fail loudly. Guessing a selector
// produces a call to whatever function happens to share those four bytes.
func TestAnUnknownSignatureIsRefused(t *testing.T) {
	_, err := (&RPCChain{RPC: liveRPC}).Call(
		context.Background(), "0x0000000000000000000000000000000000000001",
		"somethingNobodyAdded()(uint256)")
	if err == nil || !strings.Contains(err.Error(), "no selector known") {
		t.Fatalf("expected a refusal naming the missing selector, got %v", err)
	}
}
