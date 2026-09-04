package margin

import (
	"fmt"
	"sort"
)

// LaneUse is what one lane reports about an account: how much of its budget the
// account's open positions and resting orders actually require.
//
// Reported by the lane, never inferred here. The lane owns the margin
// arithmetic, and a second implementation of it in the control plane is a
// second implementation that can disagree with the one holding the collateral.
type LaneUse struct {
	Lane uint32
	// Required is margin the lane's positions and orders currently need.
	Required int64
	// Budget is what the ledger has allocated to it.
	Budget int64
}

// Surplus is budget the lane is not using. Never negative: a lane using more
// than its budget is a lane that was funded before the ledger knew, not a lane
// with negative surplus, and the difference matters when it is subtracted.
func (u LaneUse) Surplus() int64 {
	if u.Budget <= u.Required {
		return 0
	}
	return u.Budget - u.Required
}

// Move is one step of a rebalance.
type Move struct {
	From   uint32 // 0 means the free pool
	To     uint32 // 0 means the free pool
	Amount int64
}

func (m Move) String() string {
	name := func(l uint32) string {
		if l == 0 {
			return "free"
		}
		return fmt.Sprintf("lane %d", l)
	}
	return fmt.Sprintf("%s -> %s: %d", name(m.From), name(m.To), m.Amount)
}

// Plan works out how to fund a lane from everything the account is not using.
//
// Returns the moves rather than performing them, for two reasons. Each release
// has to be confirmed by the lane it takes from -- the ledger's view of what is
// free is a belief, and the lane is the authority -- so the caller has to be
// able to do them one at a time and stop. And a plan that cannot be fully
// funded should be visible as such before any of it is executed, rather than
// half-applied and then abandoned.
//
// Cheapest source first: the free pool, then the largest surplus. Draining many
// small lanes to fund one is more releases, and every release is a round trip
// to a lane that might refuse.
func Plan(l *Ledger, k Key, want uint32, need int64, use []LaneUse) ([]Move, error) {
	if need <= 0 {
		return nil, nil
	}
	free := l.Free(k)
	var moves []Move
	remaining := need

	if free > 0 {
		take := min64(free, remaining)
		moves = append(moves, Move{From: 0, To: want, Amount: take})
		remaining -= take
	}
	if remaining == 0 {
		return moves, nil
	}

	// Largest surplus first, and never from the lane being funded.
	donors := make([]LaneUse, 0, len(use))
	for _, u := range use {
		if u.Lane != want && u.Surplus() > 0 {
			donors = append(donors, u)
		}
	}
	sort.Slice(donors, func(i, j int) bool {
		if donors[i].Surplus() != donors[j].Surplus() {
			return donors[i].Surplus() > donors[j].Surplus()
		}
		return donors[i].Lane < donors[j].Lane // deterministic on ties
	})

	for _, d := range donors {
		if remaining == 0 {
			break
		}
		take := min64(d.Surplus(), remaining)
		moves = append(moves,
			Move{From: d.Lane, To: 0, Amount: take},
			Move{From: 0, To: want, Amount: take})
		remaining -= take
	}

	if remaining > 0 {
		return moves, fmt.Errorf(
			"margin: %d short of %d for lane %d; %d free and %d recoverable from other lanes",
			remaining, need, want, free, need-remaining-free)
	}
	return moves, nil
}

// Apply performs a plan against the ledger.
//
// Stops at the first failure and reports what was already done, because the
// alternative is a caller that believes a plan either fully happened or did not
// happen at all. Releases against a lane must already have been confirmed with
// that lane; this only moves the ledger's own numbers.
func Apply(l *Ledger, k Key, moves []Move) (done int, err error) {
	for i, m := range moves {
		switch {
		case m.From == 0 && m.To != 0:
			err = l.Allocate(k, m.To, m.Amount)
		case m.From != 0 && m.To == 0:
			err = l.Release(k, m.From, m.Amount)
		default:
			err = fmt.Errorf("margin: a move goes %s, which is not free-to-lane or lane-to-free", m)
		}
		if err != nil {
			return i, fmt.Errorf("move %d (%s): %w", i, m, err)
		}
	}
	return len(moves), nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
