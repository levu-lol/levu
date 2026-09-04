// Package margin lets one deposit back positions in several lanes.
//
// A trader is one person; lanes are not. Funding each market separately is what
// the per-market decomposition costs, and this is the part of it that can be
// bought back without a second chain: a single balance, parcelled out to lanes,
// rebalanced as positions change.
//
// It is deliberately *allocated* margin rather than true cross-margin. Checking
// margin on an order needs every position synchronously, and a lane that had to
// ask across a boundary before matching would give up the latency that made
// per-market lanes worth having. So each lane is given a budget it enforces
// locally, and the budget moves between lanes when it is free to.
//
// The whole package exists to protect one invariant:
//
//	sum(allocated to every lane) + free == deposited
//
// Breaking it means the same collateral backing two positions in two lanes, and
// discovering that during a liquidation rather than here. Every exported method
// re-establishes it and TestTheInvariantHolds hammers it.
package margin

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Asset is the denomination a balance is held in. Margin is never fungible
// across denominations: a USDG balance cannot back an ETH-quoted lane, because
// settling would release an asset the lane never took in.
type Asset string

const (
	USDG Asset = "usdg"
	ETH  Asset = "eth"
	// TUSDG is the unit of account on our own layer, and it is deliberately
	// not USDG.
	//
	// Nothing bridges it. It is minted by a faucet, it settles nothing on
	// Robinhood Chain, and a balance of it is worth exactly as much as the
	// person holding it paid for it, which is nothing. Naming it tUSDG rather
	// than reusing USDG is the whole point: a paper balance labelled with a
	// real asset's ticker is one screenshot away from being mistaken for the
	// real asset, and the moment a bridge does exist the two must not have
	// been sharing a name.
	//
	// One denomination, not one per quote token. Lanes here are valued in USD
	// whatever their pool is quoted in, so a single balance backs all of them
	// and shared margin has something to share.
	TUSDG Asset = "tusdg"
)

// Key identifies one trader's balance in one denomination.
type Key struct {
	Account [20]byte
	Asset   Asset
}

var (
	// ErrInsufficientFree means the allocation would exceed the deposit.
	ErrInsufficientFree = errors.New("margin: not enough free balance")
	// ErrOverRelease means more was released than the lane holds, which would
	// invent collateral.
	ErrOverRelease = errors.New("margin: released more than the lane holds")
)

type balance struct {
	deposited int64
	byLane    map[uint32]int64
}

func (b *balance) allocated() int64 {
	var n int64
	for _, v := range b.byLane {
		n += v
	}
	return n
}

// Ledger is the authority on who has what and where it is.
//
// One authority, deliberately. Two things able to allocate the same balance is
// the double-spend this package exists to prevent, and the cheapest way to not
// have that race is to not have two of these.
type Ledger struct {
	mu sync.RWMutex
	by map[Key]*balance
}

func New() *Ledger { return &Ledger{by: map[Key]*balance{}} }

func (l *Ledger) get(k Key) *balance {
	b, ok := l.by[k]
	if !ok {
		b = &balance{byLane: map[uint32]int64{}}
		l.by[k] = b
	}
	return b
}

// Deposit adds to a trader's balance. Not allocated to anything yet.
func (l *Ledger) Deposit(k Key, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("margin: deposit must be positive, got %d", amount)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(k)
	if b.deposited > (1<<62)-amount {
		return fmt.Errorf("margin: deposit would overflow the balance")
	}
	b.deposited += amount
	return nil
}

// Allocate moves free balance into a lane's budget.
//
// Refused rather than clamped when there is not enough. A silent partial
// allocation would leave the caller believing a lane is funded for a position it
// cannot actually carry, which is worse than a failed order.
func (l *Ledger) Allocate(k Key, lane uint32, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("margin: allocation must be positive, got %d", amount)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(k)
	if free := b.deposited - b.allocated(); free < amount {
		return fmt.Errorf("%w: %d free, %d requested", ErrInsufficientFree, free, amount)
	}
	b.byLane[lane] += amount
	return nil
}

// Release returns a lane's budget to the free pool.
//
// The caller must have confirmed with the lane that the margin is genuinely
// unused. This is the ledger's view; the lane is the authority on what its
// positions require, and releasing margin a position still needs would be
// discovered at the next liquidation rather than here.
func (l *Ledger) Release(k Key, lane uint32, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("margin: release must be positive, got %d", amount)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(k)
	held := b.byLane[lane]
	if held < amount {
		return fmt.Errorf("%w: lane %d holds %d, released %d",
			ErrOverRelease, lane, held, amount)
	}
	b.byLane[lane] = held - amount
	if b.byLane[lane] == 0 {
		delete(b.byLane, lane)
	}
	return nil
}

// Withdraw removes free balance entirely.
//
// Only free balance: collateral allocated to a lane is backing a position until
// that lane says otherwise, and letting it leave here would be the double-spend
// running in reverse.
func (l *Ledger) Withdraw(k Key, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("margin: withdrawal must be positive, got %d", amount)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(k)
	free := b.deposited - b.allocated()
	if free < amount {
		return fmt.Errorf("%w: %d free, %d requested (%d is allocated to lanes)",
			ErrInsufficientFree, free, amount, b.allocated())
	}
	b.deposited -= amount
	return nil
}

// Free is what may be allocated or withdrawn right now.
func (l *Ledger) Free(k Key) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	b, ok := l.by[k]
	if !ok {
		return 0
	}
	return b.deposited - b.allocated()
}

// Deposited is the whole balance, wherever it currently sits.
func (l *Ledger) Deposited(k Key) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if b, ok := l.by[k]; ok {
		return b.deposited
	}
	return 0
}

// AllocatedTo is this lane's budget.
func (l *Ledger) AllocatedTo(k Key, lane uint32) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if b, ok := l.by[k]; ok {
		return b.byLane[lane]
	}
	return 0
}

// Lanes lists where this balance currently sits, lowest lane id first.
func (l *Ledger) Lanes(k Key) []uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	b, ok := l.by[k]
	if !ok {
		return nil
	}
	out := make([]uint32, 0, len(b.byLane))
	for lane := range b.byLane {
		out = append(out, lane)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Check re-derives the invariant and reports any balance that breaks it.
//
// Cheap, and worth running: this package's whole job is one equality, and an
// equality nobody checks is a comment.
func (l *Ledger) Check() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for k, b := range l.by {
		if b.allocated() > b.deposited {
			return fmt.Errorf(
				"margin: %x/%s has %d allocated against %d deposited: "+
					"the same collateral is backing two lanes",
				k.Account[:4], k.Asset, b.allocated(), b.deposited)
		}
		for lane, v := range b.byLane {
			if v < 0 {
				return fmt.Errorf("margin: %x/%s lane %d holds %d",
					k.Account[:4], k.Asset, lane, v)
			}
		}
	}
	return nil
}
