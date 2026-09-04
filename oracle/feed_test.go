package oracle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/levu-lol/levu/wire"
)

func venue(name string, price, liq int64) StaticVenue {
	return StaticVenue{
		VenueName: name,
		Price:     wire.FixedWhole(price),
		Liquidity: wire.FixedWhole(liq),
	}
}

func feedAt(venues ...Venue) *Feed {
	cfg := DefaultFeedConfig()
	cfg.VenueTimeout = 200 * time.Millisecond
	return NewFeed(venues, cfg)
}

var readAt = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func TestAReadingAggregatesEveryVenueThatAnswered(t *testing.T) {
	f := feedAt(
		venue("native-pool", 100, 2_000_000),
		venue("aggregator", 100, 1_000_000),
		venue("external", 101, 900_000),
	)
	r := f.Read(context.Background(), readAt)

	if !r.Usable() {
		t.Fatalf("reading unusable: %+v", r.Result)
	}
	if len(r.Failed) != 0 {
		t.Errorf("unexpected failures: %+v", r.Failed)
	}
	if len(r.Result.Used) != 3 {
		t.Errorf("used %v, want three venues", r.Result.Used)
	}
	if r.Result.Price.Cmp(wire.FixedWhole(100)) != 0 {
		t.Errorf("price = %s, want 100", r.Result.Price)
	}
}

// / The property the whole design rests on: a venue going away does not break
// / the price, it lowers confidence. Confidence gates leverage in the VM, so an
// / outage tightens the market on its own with no special case anywhere.
func TestALostVenueLowersConfidenceRatherThanBreakingTheReading(t *testing.T) {
	healthy := feedAt(
		venue("native-pool", 100, 2_000_000),
		venue("aggregator", 100, 1_000_000),
		venue("external", 100, 900_000),
	)
	degraded := feedAt(
		venue("native-pool", 100, 2_000_000),
		venue("aggregator", 100, 1_000_000),
		StaticVenue{VenueName: "external", Err: errors.New("connection refused")},
	)

	h := healthy.Read(context.Background(), readAt)
	d := degraded.Read(context.Background(), readAt)

	if !d.Usable() {
		t.Fatal("losing one venue of three must not make the price unusable")
	}
	if d.Result.Price.Cmp(h.Result.Price) != 0 {
		t.Errorf("the price moved because a venue vanished: %s vs %s",
			h.Result.Price, d.Result.Price)
	}
	if d.Result.Confidence >= h.Result.Confidence {
		t.Errorf("confidence did not fall: %d -> %d", h.Result.Confidence, d.Result.Confidence)
	}
	if len(d.Failed) != 1 || d.Failed[0].Venue != "external" {
		t.Errorf("the failure should be reported: %+v", d.Failed)
	}
}

// / One slow venue must not stall the reading. It is dropped on timeout and the
// / rest are aggregated — a late price is worse than a missing one, because the
// / VM can see a missing price and cannot see a late one.
func TestASlowVenueIsDroppedRatherThanWaitedFor(t *testing.T) {
	f := feedAt(
		venue("fast-a", 100, 2_000_000),
		venue("fast-b", 100, 1_500_000),
		StaticVenue{
			VenueName: "molasses", Delay: 3 * time.Second,
			Price: wire.FixedWhole(100), Liquidity: wire.FixedWhole(9_000_000),
		},
	)
	start := time.Now()
	r := f.Read(context.Background(), readAt)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("read took %v; the slow venue was waited for", elapsed)
	}
	if !r.Usable() {
		t.Fatal("the two fast venues should still produce a price")
	}
	if len(r.Failed) != 1 || r.Failed[0].Venue != "molasses" {
		t.Errorf("timeout not reported: %+v", r.Failed)
	}
}

func TestEveryVenueFailingYieldsNoPrice(t *testing.T) {
	down := errors.New("down")
	f := feedAt(
		StaticVenue{VenueName: "a", Err: down},
		StaticVenue{VenueName: "b", Err: down},
	)
	r := f.Read(context.Background(), readAt)
	if r.Usable() {
		t.Error("a reading with no sources must not be usable")
	}
	if len(r.Failed) != 2 {
		t.Errorf("failures = %+v", r.Failed)
	}
	if r.Result.Confidence != 0 {
		t.Errorf("confidence = %d, want 0", r.Result.Confidence)
	}
}

// / Two nodes reading the same venues must agree, so the reading cannot depend
// / on which goroutine finished first.
func TestReadingsAreDeterministicAcrossPollOrder(t *testing.T) {
	mk := func() *Feed {
		return feedAt(
			StaticVenue{VenueName: "slow", Delay: 40 * time.Millisecond,
				Price: wire.FixedWhole(101), Liquidity: wire.FixedWhole(1_000_000)},
			StaticVenue{VenueName: "quick", Price: wire.FixedWhole(100),
				Liquidity: wire.FixedWhole(2_000_000)},
			StaticVenue{VenueName: "middling", Delay: 15 * time.Millisecond,
				Price: wire.FixedWhole(100), Liquidity: wire.FixedWhole(1_500_000)},
		)
	}
	base := mk().Read(context.Background(), readAt)
	for i := 0; i < 8; i++ {
		got := mk().Read(context.Background(), readAt)
		if got.Result.Price.Cmp(base.Result.Price) != 0 ||
			got.Result.Confidence != base.Result.Confidence {
			t.Fatalf("run %d differed: %s/%d vs %s/%d", i,
				got.Result.Price, got.Result.Confidence,
				base.Result.Price, base.Result.Confidence)
		}
	}
}

func TestLastReportsTheMostRecentReading(t *testing.T) {
	f := feedAt(venue("a", 100, 2_000_000), venue("b", 100, 2_000_000))
	if _, ok := f.Last(); ok {
		t.Error("there should be no reading before the first poll")
	}
	f.Read(context.Background(), readAt)
	last, ok := f.Last()
	if !ok || !last.Usable() {
		t.Errorf("last = %+v, ok = %v", last, ok)
	}
}

// / A consumer that falls behind should get the next reading, not a backlog of
// / stale ones — the VM's staleness check can see a missing price and cannot see
// / a queued one.
func TestRunDropsReadingsRatherThanQueueingThem(t *testing.T) {
	cfg := DefaultFeedConfig()
	cfg.Interval = 5 * time.Millisecond
	cfg.VenueTimeout = 100 * time.Millisecond
	f := NewFeed([]Venue{venue("a", 100, 2_000_000), venue("b", 100, 2_000_000)}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Millisecond)
	defer cancel()
	out := make(chan Reading, 1)
	done := make(chan struct{})
	go func() { f.Run(ctx, out); close(done) }()

	<-done
	// The buffer holds one; the rest were dropped rather than accumulating.
	if len(out) > 1 {
		t.Errorf("%d readings queued; they should have been dropped", len(out))
	}
	if _, ok := f.Last(); !ok {
		t.Error("Run should have recorded a reading even with nobody listening")
	}
}

// / A manipulated venue is still rejected once it is inside a feed, not just in
// / a unit test of the aggregator.
func TestAnOutlierVenueIsExcludedFromALiveReading(t *testing.T) {
	f := feedAt(
		venue("native-pool", 100, 2_000_000),
		venue("aggregator", 101, 1_500_000),
		venue("attacker", 1_000, 5_000),
	)
	r := f.Read(context.Background(), readAt)
	for _, u := range r.Result.Used {
		if u == "attacker" {
			t.Fatal("the manipulated venue was aggregated")
		}
	}
	if r.Result.Price.Cmp(wire.FixedWhole(101)) > 0 {
		t.Errorf("price = %s; the outlier moved it", r.Result.Price)
	}
}
