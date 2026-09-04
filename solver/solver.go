// Package solver auctions residual hedge flow.
//
// A lane's fills mostly cross internally. What is left — the inventory its
// market makers are carrying — has to be offset in spot, and that residual is
// small relative to the volume that produced it. Auctioning it rather than
// routing it to a fixed venue turns the compression into a price: several
// solvers compete to execute the same flow, and whoever can source it cheapest
// wins.
//
// The auction is deliberately simple and deterministic. Every rule here exists
// because the alternative is exploitable:
//
//   - Best price wins, ties broken by name, so the same bids always produce the
//     same award and a solver cannot gain by re-submitting.
//   - A bid worse than the reference is refused outright. The point of the
//     auction is to beat executing against the reference venue, not to dress up
//     a worse fill as a competitive one.
//   - Bids must be bonded. An unbonded quote costs nothing to make and nothing
//     to abandon, so it is not a quote.
package solver

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/levu-lol/levu/wire"
)

// HedgeRequest is residual inventory that must be offset off-venue.
type HedgeRequest struct {
	MarketID uint32
	Symbol   string
	// Size is signed in base units: positive means spot must be bought.
	Size wire.Fixed
	// Reference is the price the auction is judged against — what executing
	// against the venue's own spot pool would cost.
	Reference wire.Fixed
	Deadline  time.Time
}

// Buying reports the direction of the hedge.
func (h HedgeRequest) Buying() bool { return h.Size.IsPositive() }

// Bid is one solver's offer to execute the whole request.
type Bid struct {
	Solver string
	// Price the solver commits to execute at, in quote per base.
	Price wire.Fixed
	// Bond backing the commitment. A quote nobody can be made to honour is not
	// a quote.
	Bond      wire.Fixed
	Submitted time.Time
}

// Split determines who keeps the price improvement.
//
// Most of it goes back to the lane — the improvement exists because the lane's
// flow was worth competing for, so the traders who generated it should see it.
// The protocol's share is what pays for running the auction.
type Split struct {
	ToLane     wire.Fixed
	ToProtocol wire.Fixed
}

func DefaultSplit() Split {
	return Split{
		ToLane:     wire.FixedRawInt64(800_000_000_000_000_000), // 0.8
		ToProtocol: wire.FixedRawInt64(200_000_000_000_000_000), // 0.2
	}
}

func (s Split) validate() error {
	sum := s.ToLane.Add(s.ToProtocol)
	if sum.Cmp(wire.FixedOne()) != 0 {
		return fmt.Errorf("solver: split must sum to one, got %s", sum)
	}
	return nil
}

// Config tunes the auction.
type Config struct {
	// MinBond a solver must post for its bid to be considered.
	MinBond wire.Fixed
	Split   Split
}

func DefaultConfig() Config {
	return Config{MinBond: wire.FixedWhole(10_000), Split: DefaultSplit()}
}

// Award is the outcome of an auction.
type Award struct {
	Request HedgeRequest
	Winner  string
	Price   wire.Fixed
	// Improvement in quote units against executing at the reference price.
	Improvement wire.Fixed
	ToLane      wire.Fixed
	ToProtocol  wire.Fixed
	// Rejected records why each discarded bid lost, so a solver can be told.
	Rejected []Rejection
}

type Rejection struct {
	Solver string
	Reason string
}

var (
	ErrNoBids     = errors.New("solver: no eligible bids")
	ErrEmptyHedge = errors.New("solver: nothing to hedge")
	ErrBadConfig  = errors.New("solver: invalid configuration")
	ErrExpired    = errors.New("solver: request deadline passed")
)

// Auction picks the best bid and computes what the improvement is worth.
func Auction(req HedgeRequest, bids []Bid, cfg Config, now time.Time) (*Award, error) {
	if err := cfg.Split.validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrBadConfig, err)
	}
	if req.Size.IsZero() {
		return nil, ErrEmptyHedge
	}
	if !req.Reference.IsPositive() {
		return nil, fmt.Errorf("%w: reference price must be positive", ErrBadConfig)
	}
	if !req.Deadline.IsZero() && now.After(req.Deadline) {
		return nil, ErrExpired
	}

	award := &Award{Request: req}
	buying := req.Buying()

	eligible := make([]Bid, 0, len(bids))
	for _, b := range bids {
		switch {
		case !b.Price.IsPositive():
			award.Rejected = append(award.Rejected, Rejection{b.Solver, "non-positive price"})
		case b.Bond.Cmp(cfg.MinBond) < 0:
			award.Rejected = append(award.Rejected,
				Rejection{b.Solver, fmt.Sprintf("bond %s below the %s floor", b.Bond, cfg.MinBond)})
		case buying && b.Price.Cmp(req.Reference) > 0:
			// Paying more than the reference is worse than not holding an
			// auction at all.
			award.Rejected = append(award.Rejected,
				Rejection{b.Solver, "price worse than the reference"})
		case !buying && b.Price.Cmp(req.Reference) < 0:
			award.Rejected = append(award.Rejected,
				Rejection{b.Solver, "price worse than the reference"})
		default:
			eligible = append(eligible, b)
		}
	}
	if len(eligible) == 0 {
		return award, ErrNoBids
	}

	// Best price first; ties broken by name so the award is reproducible.
	sort.SliceStable(eligible, func(i, j int) bool {
		c := eligible[i].Price.Cmp(eligible[j].Price)
		if c != 0 {
			if buying {
				return c < 0 // buying: cheaper is better
			}
			return c > 0 // selling: dearer is better
		}
		return eligible[i].Solver < eligible[j].Solver
	})
	best := eligible[0]
	for _, b := range eligible[1:] {
		award.Rejected = append(award.Rejected, Rejection{b.Solver, "outbid"})
	}

	// Improvement is always signed so that positive means better than the
	// reference, whichever way the hedge goes.
	var perUnit wire.Fixed
	if buying {
		perUnit = req.Reference.Sub(best.Price)
	} else {
		perUnit = best.Price.Sub(req.Reference)
	}
	improvement := perUnit.Mul(req.Size.Abs())

	award.Winner = best.Solver
	award.Price = best.Price
	award.Improvement = improvement
	award.ToLane = improvement.Mul(cfg.Split.ToLane)
	// The protocol takes the remainder, so no unit is lost to rounding.
	award.ToProtocol = improvement.Sub(award.ToLane)
	return award, nil
}
