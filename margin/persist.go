package margin

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Snapshot is the ledger in a form that survives a restart.
//
// Only deposits and lane allocations, because that is the whole of what this
// package is the authority on. Positions live in the lanes and come back
// through their own checkpoints; what must not be lost here is how much a
// trader has and which lanes are currently holding it, because losing that
// silently invents or destroys collateral.
type Snapshot struct {
	Balances []SnapshotBalance `json:"balances"`
}

type SnapshotBalance struct {
	Account   string           `json:"account"` // 0x-prefixed, 20 bytes
	Asset     Asset            `json:"asset"`
	Deposited int64            `json:"deposited"`
	ByLane    map[uint32]int64 `json:"by_lane,omitempty"`
}

// Save renders the ledger. Safe to call while it is in use.
func (l *Ledger) Save() Snapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := Snapshot{Balances: make([]SnapshotBalance, 0, len(l.by))}
	for k, b := range l.by {
		lanes := make(map[uint32]int64, len(b.byLane))
		for lane, v := range b.byLane {
			if v != 0 {
				lanes[lane] = v
			}
		}
		out.Balances = append(out.Balances, SnapshotBalance{
			Account:   "0x" + hex.EncodeToString(k.Account[:]),
			Asset:     k.Asset,
			Deposited: b.deposited,
			ByLane:    lanes,
		})
	}
	return out
}

// Load replaces the ledger's contents.
//
// It refuses a snapshot that does not satisfy the invariant rather than
// loading it and letting Check fail later somewhere unrelated: a ledger whose
// allocations exceed its deposits is not a ledger, and the moment to say so is
// while there is still a stack trace pointing at the file that produced it.
func (l *Ledger) Load(s Snapshot) error {
	next := make(map[Key]*balance, len(s.Balances))
	for i, sb := range s.Balances {
		raw, err := hex.DecodeString(trim0x(sb.Account))
		if err != nil || len(raw) != 20 {
			return fmt.Errorf("margin: balance %d has a bad account %q", i, sb.Account)
		}
		if sb.Deposited < 0 {
			return fmt.Errorf("margin: balance %d has a negative deposit", i)
		}
		var k Key
		copy(k.Account[:], raw)
		k.Asset = sb.Asset
		if _, dup := next[k]; dup {
			return fmt.Errorf("margin: %s appears twice for %s", sb.Account, sb.Asset)
		}
		b := &balance{deposited: sb.Deposited, byLane: map[uint32]int64{}}
		var alloc int64
		for lane, v := range sb.ByLane {
			if v < 0 {
				return fmt.Errorf("margin: %s has a negative allocation to lane %d", sb.Account, lane)
			}
			b.byLane[lane] = v
			alloc += v
		}
		if alloc > b.deposited {
			return fmt.Errorf("margin: %s allocates %d of %d deposited",
				sb.Account, alloc, b.deposited)
		}
		next[k] = b
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.by = next
	return nil
}

func trim0x(s string) string {
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}

// MarshalJSON / UnmarshalJSON keep the file shape in one place.
func (s Snapshot) Bytes() ([]byte, error) { return json.MarshalIndent(s, "", " ") }
