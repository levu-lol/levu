package oracle

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/levu-lol/levu/wire"
)

// Venue is one place a price can be read from.
//
// The interface is deliberately thin — a name, a quote, and an error — because
// what varies between a pool, a centralised venue and a price API is entirely
// in how the quote is obtained, and none of that should leak into aggregation.
type Venue interface {
	Name() string
	// Quote returns the venue's current price and the depth standing behind it.
	Quote(ctx context.Context) (Source, error)
}

// FeedConfig tunes polling.
type FeedConfig struct {
	// Interval between readings.
	Interval time.Duration
	// Timeout for a single venue. One slow venue must not stall the reading:
	// it is dropped, coverage falls, and confidence falls with it.
	VenueTimeout time.Duration
	// Aggregation rules.
	Aggregate Config
}

func DefaultFeedConfig() FeedConfig {
	return FeedConfig{
		Interval: 2 * time.Second,
		// Sized for a shared, rate-limited endpoint rather than a dedicated
		// node. Too tight and every venue is dropped on timeout, which reads as
		// a market with no price sources rather than as a slow connection.
		VenueTimeout: 5 * time.Second,
		Aggregate:    DefaultConfig(),
	}
}

// Reading is one aggregated observation of a market, with the venue-level
// detail that produced it retained so a low confidence can be explained.
type Reading struct {
	At     time.Time
	Result Result
	// Failed records venues that could not be read at all, as distinct from
	// venues that answered and were then rejected by the aggregator.
	Failed []VenueFailure
	// Latency of the slowest venue that answered.
	Latency time.Duration
	// Sources that answered, before the aggregator filtered them.
	//
	// Kept because a consumer almost always wants the per-venue detail it
	// already paid to fetch — for display, and for depth, which a venue reports
	// alongside its price. Re-reading the pools to recover it doubles the round
	// trips for data already in hand.
	Sources []Source
}

type VenueFailure struct {
	Venue string
	Err   string
}

// Usable reports whether the reading may be pushed to a lane.
func (r Reading) Usable() bool { return r.Result.Healthy && r.Result.Price.IsPositive() }

// Feed polls venues and aggregates them.
//
// The important property is what happens when a venue goes away: it is dropped
// from the reading, coverage falls, and the confidence the aggregator reports
// falls with it. Confidence gates leverage in the VM, so a venue outage tightens
// the market automatically rather than producing a confident wrong price. That
// is the whole design — degradation is a continuous slope, not a cliff, and it
// needs no special case anywhere.
type Feed struct {
	venues []Venue
	cfg    FeedConfig

	mu   sync.RWMutex
	last Reading
	ok   bool
}

func NewFeed(venues []Venue, cfg FeedConfig) *Feed {
	return &Feed{venues: venues, cfg: cfg}
}

// Read polls every venue once and aggregates the answers.
func (f *Feed) Read(ctx context.Context, now time.Time) Reading {
	type outcome struct {
		src Source
		err error
		dur time.Duration
	}
	results := make([]outcome, len(f.venues))
	var wg sync.WaitGroup

	for i, v := range f.venues {
		wg.Add(1)
		go func(i int, v Venue) {
			defer wg.Done()
			vctx := ctx
			if f.cfg.VenueTimeout > 0 {
				var cancel context.CancelFunc
				vctx, cancel = context.WithTimeout(ctx, f.cfg.VenueTimeout)
				defer cancel()
			}
			start := time.Now()
			src, err := v.Quote(vctx)
			results[i] = outcome{src: src, err: err, dur: time.Since(start)}
			if err == nil && results[i].src.Name == "" {
				results[i].src.Name = v.Name()
			}
			if err == nil && results[i].src.Observed.IsZero() {
				results[i].src.Observed = now
			}
		}(i, v)
	}
	wg.Wait()

	r := Reading{At: now}
	sources := make([]Source, 0, len(results))
	for i, o := range results {
		if o.dur > r.Latency {
			r.Latency = o.dur
		}
		if o.err != nil {
			r.Failed = append(r.Failed, VenueFailure{Venue: f.venues[i].Name(), Err: o.err.Error()})
			continue
		}
		sources = append(sources, o.src)
	}
	// Deterministic order in, deterministic reading out.
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	r.Sources = sources
	r.Result = Aggregate(sources, f.cfg.Aggregate, now)

	f.mu.Lock()
	f.last, f.ok = r, true
	f.mu.Unlock()
	return r
}

// Last returns the most recent reading, and whether there has been one.
func (f *Feed) Last() (Reading, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.last, f.ok
}

// Run polls on the configured interval until the context is done, delivering
// each reading to `out`.
//
// Sends are non-blocking. A consumer that falls behind gets the next reading
// rather than a stale queue — an old price is worse than a missing one, because
// the VM's staleness check can see a missing price and cannot see a queued one.
func (f *Feed) Run(ctx context.Context, out chan<- Reading) {
	t := time.NewTicker(f.cfg.Interval)
	defer t.Stop()
	for {
		r := f.Read(ctx, time.Now().UTC())
		select {
		case out <- r:
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// StaticVenue is a fixed quote, for tests and for pinning a reference price.
type StaticVenue struct {
	VenueName string
	Price     wire.Fixed
	Liquidity wire.Fixed
	Err       error
	Delay     time.Duration
}

func (s StaticVenue) Name() string { return s.VenueName }

func (s StaticVenue) Quote(ctx context.Context) (Source, error) {
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-ctx.Done():
			return Source{}, fmt.Errorf("%s: %w", s.VenueName, ctx.Err())
		}
	}
	if s.Err != nil {
		return Source{}, s.Err
	}
	return Source{Name: s.VenueName, Price: s.Price, Liquidity: s.Liquidity}, nil
}
