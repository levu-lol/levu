package solver

import (
	"errors"
	"testing"
	"time"

	"github.com/levu-lol/levu/wire"
)

var now = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func buyRequest(size int64) HedgeRequest {
	return HedgeRequest{
		MarketID: 1, Symbol: "PEPE",
		Size:      wire.FixedWhole(size),
		Reference: wire.FixedWhole(100),
	}
}

func sellRequest(size int64) HedgeRequest {
	r := buyRequest(size)
	r.Size = wire.FixedWhole(-size)
	return r
}

func bid(name string, price int64, bond int64) Bid {
	return Bid{Solver: name, Price: wire.FixedWhole(price), Bond: wire.FixedWhole(bond)}
}

func TestTheCheapestBidWinsAHedgePurchase(t *testing.T) {
	a, err := Auction(buyRequest(100), []Bid{
		bid("alpha", 99, 50_000),
		bid("beta", 97, 50_000),
		bid("gamma", 98, 50_000),
	}, DefaultConfig(), now)
	if err != nil {
		t.Fatalf("auction: %v", err)
	}
	if a.Winner != "beta" {
		t.Errorf("winner = %s, want beta", a.Winner)
	}
	// 100 units at 3 below the reference.
	if a.Improvement.Cmp(wire.FixedWhole(300)) != 0 {
		t.Errorf("improvement = %s, want 300", a.Improvement)
	}
}

// Selling inverts the comparison: the best bid is the highest one.
func TestTheDearestBidWinsAHedgeSale(t *testing.T) {
	a, err := Auction(sellRequest(100), []Bid{
		bid("alpha", 101, 50_000),
		bid("beta", 103, 50_000),
		bid("gamma", 102, 50_000),
	}, DefaultConfig(), now)
	if err != nil {
		t.Fatalf("auction: %v", err)
	}
	if a.Winner != "beta" {
		t.Errorf("winner = %s, want beta", a.Winner)
	}
	if a.Improvement.Cmp(wire.FixedWhole(300)) != 0 {
		t.Errorf("improvement = %s, want 300", a.Improvement)
	}
}

// The point of the auction is to beat the reference, not to dress up a worse
// fill as a competitive one.
func TestABidWorseThanTheReferenceIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		req  HedgeRequest
		bids []Bid
	}{
		"buying above reference":  {buyRequest(10), []Bid{bid("alpha", 105, 50_000)}},
		"selling below reference": {sellRequest(10), []Bid{bid("alpha", 95, 50_000)}},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := Auction(tc.req, tc.bids, DefaultConfig(), now)
			if !errors.Is(err, ErrNoBids) {
				t.Fatalf("err = %v, want ErrNoBids", err)
			}
			if len(a.Rejected) != 1 || a.Rejected[0].Reason != "price worse than the reference" {
				t.Errorf("rejection not reported: %+v", a.Rejected)
			}
		})
	}
}

// A quote nobody can be made to honour costs nothing to make and nothing to
// abandon.
func TestUnbondedBidsAreIneligible(t *testing.T) {
	cfg := DefaultConfig()
	a, err := Auction(buyRequest(100), []Bid{
		bid("cheap_but_unbonded", 90, 1),
		bid("honest", 98, 50_000),
	}, cfg, now)
	if err != nil {
		t.Fatalf("auction: %v", err)
	}
	if a.Winner != "honest" {
		t.Errorf("winner = %s: an unbonded bid must not win on price", a.Winner)
	}
}

// The same bids must always produce the same award, or a solver gains by
// resubmitting until the ordering favours it.
func TestTiesAreBrokenDeterministically(t *testing.T) {
	bids := []Bid{
		bid("zeta", 97, 50_000),
		bid("alpha", 97, 50_000),
		bid("mu", 97, 50_000),
	}
	for i := 0; i < 20; i++ {
		// Present them in a rotated order each time.
		rotated := append(append([]Bid{}, bids[i%3:]...), bids[:i%3]...)
		a, err := Auction(buyRequest(10), rotated, DefaultConfig(), now)
		if err != nil {
			t.Fatal(err)
		}
		if a.Winner != "alpha" {
			t.Fatalf("winner = %s, want alpha regardless of submission order", a.Winner)
		}
	}
}

// The improvement exists because the lane's flow was worth competing for, so
// most of it goes back to the lane.
func TestImprovementIsSplitWithoutLosingAUnit(t *testing.T) {
	a, err := Auction(buyRequest(100), []Bid{bid("alpha", 97, 50_000)}, DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if a.ToLane.Add(a.ToProtocol).Cmp(a.Improvement) != 0 {
		t.Errorf("split lost a unit: %s + %s != %s", a.ToLane, a.ToProtocol, a.Improvement)
	}
	if a.ToLane.Cmp(a.ToProtocol) <= 0 {
		t.Error("the lane should keep the larger share of its own price improvement")
	}
}

func TestNothingToHedgeIsNotAnAuction(t *testing.T) {
	r := buyRequest(0)
	if _, err := Auction(r, []Bid{bid("alpha", 99, 50_000)}, DefaultConfig(), now); !errors.Is(err, ErrEmptyHedge) {
		t.Errorf("err = %v, want ErrEmptyHedge", err)
	}
}

func TestNoBidsYieldsNoAward(t *testing.T) {
	a, err := Auction(buyRequest(100), nil, DefaultConfig(), now)
	if !errors.Is(err, ErrNoBids) {
		t.Errorf("err = %v, want ErrNoBids", err)
	}
	if a.Winner != "" {
		t.Error("awarded to nobody")
	}
}

func TestAnExpiredRequestIsRefused(t *testing.T) {
	r := buyRequest(100)
	r.Deadline = now.Add(-time.Minute)
	if _, err := Auction(r, []Bid{bid("alpha", 97, 50_000)}, DefaultConfig(), now); !errors.Is(err, ErrExpired) {
		t.Errorf("err = %v, want ErrExpired", err)
	}
}

func TestAnInvalidSplitIsRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Split.ToProtocol = cfg.Split.ToProtocol.Add(wire.FixedRawInt64(1))
	if _, err := Auction(buyRequest(10), []Bid{bid("a", 99, 50_000)}, cfg, now); !errors.Is(err, ErrBadConfig) {
		t.Errorf("err = %v, want ErrBadConfig", err)
	}
}

// Losers should be told why, so a solver can improve rather than guess.
func TestEveryDiscardedBidIsExplained(t *testing.T) {
	a, err := Auction(buyRequest(100), []Bid{
		bid("winner", 96, 50_000),
		bid("outbid", 98, 50_000),
		bid("unbonded", 90, 1),
		bid("expensive", 110, 50_000),
	}, DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rejected) != 3 {
		t.Fatalf("expected three explanations, got %+v", a.Rejected)
	}
	seen := map[string]string{}
	for _, r := range a.Rejected {
		seen[r.Solver] = r.Reason
	}
	for _, s := range []string{"outbid", "unbonded", "expensive"} {
		if seen[s] == "" {
			t.Errorf("%s was discarded without a reason", s)
		}
	}
}
