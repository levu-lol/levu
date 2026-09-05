package indexer

import (
	"context"
	"strings"
	"testing"
)

type countingChain struct{ slot0, liq int }

func (c *countingChain) Call(_ context.Context, _ string, sig string, _ ...string) ([]string, error) {
	switch {
	case strings.HasPrefix(sig, "slot0"):
		c.slot0++
		return []string{"79228162514264337593543950336", "0", "0", "0", "0", "0", "true"}, nil
	case strings.HasPrefix(sig, "liquidity"):
		c.liq++
		return []string{"1000000"}, nil
	}
	return nil, nil
}

func (c *countingChain) BlockNumber(ctx context.Context) (int64, error) {
	return 0, nil
}

func (c *countingChain) Logs(context.Context, string, string, int64, int64) ([]Log, error) {
	return nil, nil
}

// The price is read every time; the liquidity behind it is reused.
func TestLiquidityIsCachedAndThePriceIsNot(t *testing.T) {
	ch := &countingChain{}
	o := New(ch)
	p := V3Pool{Pool: "0xpool"}
	for i := 0; i < 5; i++ {
		if _, err := o.ReadV3Price(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if ch.slot0 != 5 {
		t.Fatalf("slot0 read %d times in 5 ticks; the price must be read every tick", ch.slot0)
	}
	if ch.liq != 1 {
		t.Fatalf("liquidity read %d times in 5 ticks; it should be cached", ch.liq)
	}
}
