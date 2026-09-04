package mm

import (
	"testing"
	"time"

	"github.com/levu-lol/levu/wire"
)

func acct(n byte) wire.Account {
	var a wire.Account
	a[19] = n
	return a
}

func goodMaker() Activity {
	return Activity{
		Account:          acct(1),
		Name:             "alpha",
		TwoSidedMillis:   Window(58 * time.Minute),
		WindowMillis:     Window(time.Hour),
		AverageSpreadBps: 8,
		DepthAtBest:      120_000,
		MakerVolume:      5_000_000,
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAGoodMakerReachesTheTopTier(t *testing.T) {
	s := Assess(goodMaker(), DefaultConfig())
	if !s.Qualified() {
		t.Fatalf("disqualified: %v", s.Disqualified)
	}
	if s.Tier != "primary" {
		t.Errorf("tier = %q (score %d), want primary", s.Tier, s.Total)
	}
	if s.MakerFeeBps >= 0 {
		t.Errorf("maker fee = %d, want a rebate", s.MakerFeeBps)
	}
}

// / The programme pays for presence and tightness, not for turnover. A maker
// / with enormous volume but who is barely quoting should score below one that
// / is always there.
func TestVolumeDoesNotBuyATier(t *testing.T) {
	cfg := DefaultConfig()

	absent := goodMaker()
	absent.Name = "absent"
	absent.MakerVolume = 500_000_000 // a hundred times the other
	absent.TwoSidedMillis = Window(31 * time.Minute)
	absent.AverageSpreadBps = 60

	present := goodMaker()
	present.Name = "present"
	present.MakerVolume = cfg.MinVolume

	a := Assess(absent, cfg)
	p := Assess(present, cfg)
	if a.Total >= p.Total {
		t.Errorf("volume outscored presence: absent %d vs present %d", a.Total, p.Total)
	}
	if a.MakerFeeBps < p.MakerFeeBps {
		t.Error("the absent maker was paid at least as well as the present one")
	}
}

func TestTighterQuotesScoreHigher(t *testing.T) {
	cfg := DefaultConfig()
	tight := goodMaker()
	tight.AverageSpreadBps = 5
	wide := goodMaker()
	wide.AverageSpreadBps = 80

	if Assess(tight, cfg).Total <= Assess(wide, cfg).Total {
		t.Error("a tighter quote must score higher")
	}
}

func TestOneSidedQuotingIsNotMakingAMarket(t *testing.T) {
	cfg := DefaultConfig()
	a := goodMaker()
	a.TwoSidedMillis = Window(20 * time.Minute) // a third of the window
	s := Assess(a, cfg)
	if s.Qualified() {
		t.Errorf("a maker present a third of the time qualified: %+v", s)
	}
}

// / Self-matching is what the VM already refuses. Observing an attempt means the
// / maker was trying to manufacture the signal the programme declines to reward.
func TestAttemptedSelfMatchingDisqualifies(t *testing.T) {
	a := goodMaker()
	a.SelfMatched = 1
	s := Assess(a, DefaultConfig())
	if s.Qualified() {
		t.Error("a self-matching maker qualified")
	}
	if s.Tier != "" || s.MakerFeeBps != 0 {
		t.Errorf("a disqualified maker was still assigned terms: %+v", s)
	}
}

func TestEachFloorIndependentlyDisqualifies(t *testing.T) {
	cfg := DefaultConfig()
	if s := Assess(goodMaker(), cfg); !s.Qualified() {
		t.Fatalf("baseline should qualify: %v", s.Disqualified)
	}
	for name, mut := range map[string]func(*Activity){
		"volume": func(a *Activity) { a.MakerVolume = cfg.MinVolume - 1 },
		"uptime": func(a *Activity) { a.TwoSidedMillis = 0 },
		"depth":  func(a *Activity) { a.DepthAtBest = cfg.MinDepthAtBest - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			a := goodMaker()
			mut(&a)
			if Assess(a, cfg).Qualified() {
				t.Errorf("%s floor did not disqualify", name)
			}
		})
	}
}

func TestUptimeIsClampedAtFull(t *testing.T) {
	a := goodMaker()
	a.TwoSidedMillis = a.WindowMillis * 3
	if got := a.UptimeBps(); got != BPS {
		t.Errorf("uptime = %d, want %d", got, BPS)
	}
	a.WindowMillis = 0
	if got := a.UptimeBps(); got != 0 {
		t.Errorf("uptime over an empty window = %d, want 0", got)
	}
}

// / Rankings must be reproducible: two nodes computing the schedule have to
// / agree on who is paid what.
func TestRankingIsDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	makers := []Activity{}
	for i := byte(1); i <= 5; i++ {
		a := goodMaker()
		a.Account, a.Name = acct(i), string(rune('a'+i))
		a.AverageSpreadBps = 8 // identical, forcing the name tie-break
		makers = append(makers, a)
	}
	base := AssessAll(makers, cfg)
	for shift := 1; shift < len(makers); shift++ {
		rotated := append(append([]Activity{}, makers[shift:]...), makers[:shift]...)
		got := AssessAll(rotated, cfg)
		for i := range got {
			if got[i].Name != base[i].Name {
				t.Fatalf("order changed the ranking at %d: %s vs %s",
					i, got[i].Name, base[i].Name)
			}
		}
	}
}

func TestMakerFeeRendersAsAVMParameter(t *testing.T) {
	// -2 bps is -0.0002 as a fraction of notional.
	if got := MakerFee(-2).String(); got != "-0.0002" {
		t.Errorf("maker fee = %s, want -0.0002", got)
	}
	if got := MakerFee(0).String(); got != "0.0" {
		t.Errorf("zero fee = %s", got)
	}
}

func TestInvalidWeightsAreRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WeightUptime += 100
	if err := cfg.Validate(); err == nil {
		t.Error("weights that do not sum to BPS must be refused")
	}
}
