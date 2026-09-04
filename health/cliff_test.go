package health

import (
	"testing"
	"time"
)

// A market with every gate satisfied, scoring between the degrade threshold and
// the lowest leverage tier. Perfectly tradeable, just not impressive.
func fadedButSound() Observation {
	return Observation{
		DepthWithin2Pct:     4_000_000, // deep: the gate is nowhere near
		BookDepthWithin2Pct: 1_300_000, // our own book, thinner than the AMM
		UnderwrittenCapital: 200_000,   // some backing, but well short of target
		Age:                 48 * time.Hour,
		TopHolderShareBps:   800,
		TopPoolShareBps:     6_000,
		OracleConfidence:    5_000, // just above the gate
		Volume24h:           10_000_000,
		SpotTVL:             5_000_000,
		MarketCap:           80_000_000,
		RealisedVolBps:      32_000,
	}
}

// spotBand is the same market gone quieter still: thin but real, scoring into
// the band where leverage comes off and trading continues at 1x.
func spotBand() Observation {
	o := fadedButSound()
	o.DepthWithin2Pct = 900_000 // well clear of the 50k gate, below the 1M target
	o.BookDepthWithin2Pct = 300_000
	o.UnderwrittenCapital = 250_000
	o.OracleConfidence = 5_000
	o.Age = 25 * time.Hour
	o.TopPoolShareBps = 7_500
	o.RealisedVolBps = 38_000
	return o
}

func TestProbeTheCliff(t *testing.T) {
	cfg := DefaultConfig()
	o := fadedButSound()
	s := Assess(o, cfg)
	c, ok := Derive(o, s, cfg)
	t.Logf("score=%d eligible=%v gates=%v", s.Total, s.Eligible(), s.GateFailures)
	t.Logf("derive ok=%v leverage=%dx openable=%v", ok, c.MaxLeverage, c.Openable)

	m := NewMachine(cfg)
	now := t0
	for i := 0; i < 30; i++ {
		o := fadedButSound()
		o.Time = now
		m.Step(o, now)
		now = now.Add(5 * time.Minute)
	}
	t.Logf("machine after 2.5h: state=%s leverage=%dx openable=%v",
		m.State(), m.Capacity().MaxLeverage, m.Capacity().Openable)
}
