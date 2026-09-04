package margin_test

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/levu-lol/levu/margin"
)

func acct(n byte) [20]byte {
	var a [20]byte
	a[19] = n
	return a
}

func key(n byte) margin.Key { return margin.Key{Account: acct(n), Asset: margin.USDG} }

// The one property this package exists for. The same collateral must never back
// two lanes, and finding that out during a liquidation is too late.
func TestTheSameCollateralCannotBackTwoLanes(t *testing.T) {
	l := margin.New()
	k := key(1)
	if err := l.Deposit(k, 1_000); err != nil {
		t.Fatal(err)
	}
	if err := l.Allocate(k, 1, 700); err != nil {
		t.Fatal(err)
	}
	// 300 free. Asking for 400 must be refused outright, not clamped: a silent
	// partial would leave the caller believing lane 2 is funded for a position
	// it cannot carry.
	err := l.Allocate(k, 2, 400)
	if !errors.Is(err, margin.ErrInsufficientFree) {
		t.Fatalf("over-allocation was allowed: %v", err)
	}
	if got := l.AllocatedTo(k, 2); got != 0 {
		t.Errorf("a refused allocation still moved %d", got)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	// Exactly the remainder is fine.
	if err := l.Allocate(k, 2, 300); err != nil {
		t.Fatalf("the exact free balance was refused: %v", err)
	}
	if l.Free(k) != 0 {
		t.Errorf("free = %d after allocating everything", l.Free(k))
	}
}

// Allocated collateral is backing a position. Letting it leave through
// withdrawal is the double-spend running backwards.
func TestAllocatedCollateralCannotBeWithdrawn(t *testing.T) {
	l := margin.New()
	k := key(1)
	_ = l.Deposit(k, 1_000)
	_ = l.Allocate(k, 1, 900)

	if err := l.Withdraw(k, 500); !errors.Is(err, margin.ErrInsufficientFree) {
		t.Fatalf("withdrew collateral that was backing a lane: %v", err)
	}
	if err := l.Withdraw(k, 100); err != nil {
		t.Fatalf("the free balance could not be withdrawn: %v", err)
	}
	if l.Deposited(k) != 900 || l.Free(k) != 0 {
		t.Errorf("deposited %d free %d after withdrawing the free 100",
			l.Deposited(k), l.Free(k))
	}
}

// Releasing more than a lane holds invents collateral.
func TestALaneCannotReleaseWhatItDoesNotHold(t *testing.T) {
	l := margin.New()
	k := key(1)
	_ = l.Deposit(k, 500)
	_ = l.Allocate(k, 1, 200)

	if err := l.Release(k, 1, 300); !errors.Is(err, margin.ErrOverRelease) {
		t.Fatalf("released more than the lane held: %v", err)
	}
	if err := l.Release(k, 2, 1); !errors.Is(err, margin.ErrOverRelease) {
		t.Fatalf("released from a lane holding nothing: %v", err)
	}
	if l.Deposited(k) != 500 {
		t.Errorf("a refused release changed the balance to %d", l.Deposited(k))
	}
}

// Funding a lane from what the account is not using elsewhere: the thing shared
// margin is actually for.
func TestFundingALaneFromIdleOnes(t *testing.T) {
	l := margin.New()
	k := key(1)
	_ = l.Deposit(k, 10_000)
	_ = l.Allocate(k, 1, 4_000) // PONS, mostly idle
	_ = l.Allocate(k, 2, 3_000) // WETH, busy
	// 3,000 still free.

	use := []margin.LaneUse{
		{Lane: 1, Required: 500, Budget: 4_000},   // 3,500 spare
		{Lane: 2, Required: 2_900, Budget: 3_000}, // 100 spare
	}
	moves, err := margin.Plan(l, k, 3, 5_000, use)
	if err != nil {
		t.Fatalf("could not fund 5,000 from 3,000 free plus 3,600 idle: %v", err)
	}
	if n, err := margin.Apply(l, k, moves); err != nil {
		t.Fatalf("applied %d of %d moves: %v", n, len(moves), err)
	}
	if got := l.AllocatedTo(k, 3); got != 5_000 {
		t.Errorf("lane 3 got %d of the 5,000 asked for", got)
	}
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
	// The deposit is unchanged: this moved collateral, it did not create any.
	if l.Deposited(k) != 10_000 {
		t.Errorf("rebalancing changed the deposit to %d", l.Deposited(k))
	}
}

// A plan that cannot be funded must say so before any of it runs.
func TestAnUnfundablePlanFailsBeforeMovingAnything(t *testing.T) {
	l := margin.New()
	k := key(1)
	_ = l.Deposit(k, 1_000)
	_ = l.Allocate(k, 1, 900)

	use := []margin.LaneUse{{Lane: 1, Required: 900, Budget: 900}} // nothing spare
	_, err := margin.Plan(l, k, 2, 5_000, use)
	if err == nil {
		t.Fatal("a plan for 5,000 out of 100 reported success")
	}
	if l.AllocatedTo(k, 2) != 0 {
		t.Error("a failed plan moved collateral anyway")
	}
}

// A lane using more than its budget must not read as having negative surplus,
// because that number gets subtracted.
func TestAnOverdrawnLaneOffersNoSurplus(t *testing.T) {
	u := margin.LaneUse{Lane: 1, Required: 900, Budget: 400}
	if s := u.Surplus(); s != 0 {
		t.Fatalf("surplus = %d, want 0; a negative would be added to the pool", s)
	}
}

// Margin is not fungible across denominations: releasing USDG to back an
// ETH-quoted lane would settle an asset the lane never took in.
func TestDenominationsAreSeparateBalances(t *testing.T) {
	l := margin.New()
	usdg := margin.Key{Account: acct(1), Asset: margin.USDG}
	eth := margin.Key{Account: acct(1), Asset: margin.ETH}

	_ = l.Deposit(usdg, 1_000)
	if l.Free(eth) != 0 {
		t.Fatalf("a USDG deposit gave %d of ETH margin", l.Free(eth))
	}
	if err := l.Allocate(eth, 1, 100); !errors.Is(err, margin.ErrInsufficientFree) {
		t.Fatalf("USDG funded an ETH lane: %v", err)
	}
}

// The invariant under a long run of arbitrary operations, which is where a
// missed edge shows up rather than in any single case above.
func TestTheInvariantHolds(t *testing.T) {
	l := margin.New()
	rng := rand.New(rand.NewSource(7))
	keys := []margin.Key{key(1), key(2), key(3)}

	for i := 0; i < 20_000; i++ {
		k := keys[rng.Intn(len(keys))]
		lane := uint32(rng.Intn(4) + 1)
		amt := int64(rng.Intn(500) + 1)
		switch rng.Intn(4) {
		case 0:
			_ = l.Deposit(k, amt)
		case 1:
			_ = l.Allocate(k, lane, amt)
		case 2:
			_ = l.Release(k, lane, amt)
		case 3:
			_ = l.Withdraw(k, amt)
		}
		if err := l.Check(); err != nil {
			t.Fatalf("after %d operations: %v", i, err)
		}
	}

	// And the books add up exactly, not merely inside the bound.
	for _, k := range keys {
		var allocated int64
		for _, lane := range l.Lanes(k) {
			allocated += l.AllocatedTo(k, lane)
		}
		if got := allocated + l.Free(k); got != l.Deposited(k) {
			t.Errorf("allocated %d + free %d = %d, deposited %d",
				allocated, l.Free(k), got, l.Deposited(k))
		}
	}
}
