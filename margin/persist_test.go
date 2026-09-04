package margin

import "testing"

func TestSnapshotRoundTripsAllocations(t *testing.T) {
	l := New()
	k := Key{Asset: TUSDG}
	k.Account[19] = 7
	if err := l.Deposit(k, 5_000); err != nil {
		t.Fatal(err)
	}
	if err := l.Allocate(k, 3, 1_200); err != nil {
		t.Fatal(err)
	}

	restored := New()
	if err := restored.Load(l.Save()); err != nil {
		t.Fatal(err)
	}
	if got := restored.Deposited(k); got != 5_000 {
		t.Fatalf("deposited %d, want 5000", got)
	}
	if got := restored.AllocatedTo(k, 3); got != 1_200 {
		t.Fatalf("allocated %d, want 1200", got)
	}
	if got := restored.Free(k); got != 3_800 {
		t.Fatalf("free %d, want 3800", got)
	}
	if err := restored.Check(); err != nil {
		t.Fatal(err)
	}
}

// A snapshot that allocates more than it holds is refused at load, not carried
// into the ledger to fail somewhere unrelated later.
func TestLoadRefusesAnImpossibleSnapshot(t *testing.T) {
	s := Snapshot{Balances: []SnapshotBalance{{
		Account: "0x0000000000000000000000000000000000000001",
		Asset:   TUSDG, Deposited: 100, ByLane: map[uint32]int64{1: 500},
	}}}
	l := New()
	if err := l.Load(s); err == nil {
		t.Fatal("loaded a ledger allocating 500 of 100 deposited")
	}
	// And the ledger it refused is left untouched.
	if err := l.Check(); err != nil {
		t.Fatal(err)
	}
}
