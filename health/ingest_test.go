package health

import (
	"strings"
	"testing"
	"time"
)

const sampleCSV = `market,time,depth_2pct,underwritten,age_hours,top_holder_bps,top_pool_bps,oracle_confidence,volume_24h,spot_tvl,market_cap,realised_vol_bps,open_interest,collapsed_at,atomic,should_list
GOOD,2026-09-01T00:00:00Z,2000000,500000,1440,800,3000,9000,10000000,5000000,80000000,8000,0,,false,true
GOOD,2026-09-01T01:00:00Z,2000000,500000,1441,800,3000,9000,10000000,5000000,80000000,8000,0,,false,true
GOOD,2026-09-01T02:00:00Z,2000000,500000,1442,800,3000,9000,10000000,5000000,80000000,8000,0,,false,true
BAD,2026-09-01T00:00:00Z,20000,0,30,9000,9500,2000,90000000,300000,100000000,40000,0,2026-09-01T02:00:00Z,true,false
BAD,2026-09-01T01:00:00Z,20000,0,31,9000,9500,2000,90000000,300000,100000000,40000,0,2026-09-01T02:00:00Z,true,false
`

func TestLoadCSVGroupsRowsIntoSeries(t *testing.T) {
	scenarios, err := LoadCSV(strings.NewReader(sampleCSV))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("scenarios = %d, want 2", len(scenarios))
	}
	good, bad := scenarios[0], scenarios[1]
	if good.Name != "GOOD" || len(good.Obs) != 3 {
		t.Errorf("GOOD: %s with %d observations", good.Name, len(good.Obs))
	}
	if !good.ShouldList {
		t.Error("GOOD should be labelled listable")
	}
	if good.Obs[0].DepthWithin2Pct != 2_000_000 {
		t.Errorf("depth = %d", good.Obs[0].DepthWithin2Pct)
	}
	if good.Obs[0].Age != 1440*time.Hour {
		t.Errorf("age = %v", good.Obs[0].Age)
	}
	if bad.ShouldList {
		t.Error("BAD should be labelled unlistable")
	}
	if bad.CollapseAt.IsZero() || !bad.Atomic {
		t.Error("BAD's collapse metadata did not load")
	}
}

// Dwell times are measured between observations, so an out-of-order row would
// silently shorten or lengthen how long a threshold appeared to hold.
func TestLoadCSVSortsObservationsChronologically(t *testing.T) {
	shuffled := `market,time,depth_2pct,oracle_confidence,volume_24h,spot_tvl
A,2026-09-01T02:00:00Z,3,9000,1,1
A,2026-09-01T00:00:00Z,1,9000,1,1
A,2026-09-01T01:00:00Z,2,9000,1,1
`
	s, err := LoadCSV(strings.NewReader(shuffled))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(s[0].Obs); i++ {
		if s[0].Obs[i].Time.Before(s[0].Obs[i-1].Time) {
			t.Fatal("observations are not in chronological order")
		}
	}
}

func TestLoadCSVRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"missing required column": "market,time\nA,2026-09-01T00:00:00Z\n",
		"bad timestamp":           "market,time,depth_2pct,oracle_confidence,volume_24h,spot_tvl\nA,not-a-time,1,1,1,1\n",
		"empty market":            "market,time,depth_2pct,oracle_confidence,volume_24h,spot_tvl\n,2026-09-01T00:00:00Z,1,1,1,1\n",
		"no data rows":            "market,time,depth_2pct,oracle_confidence,volume_24h,spot_tvl\n",
	}
	for name, csv := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCSV(strings.NewReader(csv)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestEvaluateCountsDecisionsAgainstGroundTruth(t *testing.T) {
	o := Evaluate(Scenarios(), DefaultConfig())
	if o.FalsePositives != 0 {
		t.Errorf("%d false positives on the scenario suite", o.FalsePositives)
	}
	if o.FalseNegatives != 0 {
		t.Errorf("%d false negatives on the scenario suite", o.FalseNegatives)
	}
	if o.TruePositives+o.TrueNegatives != len(Scenarios()) {
		t.Error("decisions do not account for every scenario")
	}
}

// / Listing something that rugs costs the insurance fund and the traders in it;
// / refusing something that would have been fine costs fees. The score must
// / reflect that asymmetry, or calibration will happily trade one for the other.
func TestScorePenalisesFalsePositivesFarMoreThanFalseNegatives(t *testing.T) {
	fp := Outcome{TruePositives: 5, FalsePositives: 1}
	fn := Outcome{TruePositives: 5, FalseNegatives: 1}
	if fp.Score() >= fn.Score() {
		t.Errorf("a false positive (%d) must score worse than a false negative (%d)",
			fp.Score(), fn.Score())
	}
}

func TestCalibrateFindsAValidConfiguration(t *testing.T) {
	best, all, err := Calibrate(Scenarios(), DefaultConfig(),
		[]int64{4_500, 5_000, 5_500, 6_000, 6_500, 7_000})
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no candidates were evaluated")
	}
	if err := best.Config.Validate(); err != nil {
		t.Errorf("calibration produced an invalid configuration: %v", err)
	}
	// Every candidate must be offerable at both ends of the ladder: a listing
	// score has to land on a tier, and the bottom rung has to be 1x so a fading
	// market is downgraded rather than dropped.
	//
	// Note this is deliberately *not* ListAbove == lowest tier any more. The
	// lowest tier now sits below the listing threshold on purpose: harder to
	// get in than to stay in, which is hysteresis rather than a dead zone.
	for _, o := range all {
		if err := o.Config.Validate(); err != nil {
			t.Errorf("candidate is invalid: %v", err)
			continue
		}
		tiers := o.Config.Tiers
		if tiers[len(tiers)-1].Leverage != 1 {
			t.Errorf("candidate has no spot rung: lowest tier is %dx", tiers[len(tiers)-1].Leverage)
		}
		var entry int64
		for _, tr := range tiers {
			if o.Config.ListAbove >= tr.MinScore {
				entry = tr.Leverage
				break
			}
		}
		if entry == 0 {
			t.Errorf("a market listing at %d matches no tier", o.Config.ListAbove)
		}
		if o.Config.SpotOnlyBelow <= o.Config.DelistBelow {
			t.Errorf("candidate loses its market before its leverage: spot %d, delist %d",
				o.Config.SpotOnlyBelow, o.Config.DelistBelow)
		}
	}
	for _, o := range all {
		if o.Score() > best.Score() {
			t.Error("Calibrate did not return the highest-scoring candidate")
		}
	}
}

func TestCalibrateRefusesAnEmptySet(t *testing.T) {
	if _, _, err := Calibrate(nil, DefaultConfig(), []int64{5_500}); err == nil {
		t.Error("calibrating against no data must be an error")
	}
}

func TestLoadedSeriesCanBeCalibrated(t *testing.T) {
	scenarios, err := LoadCSV(strings.NewReader(sampleCSV))
	if err != nil {
		t.Fatal(err)
	}
	best, _, err := Calibrate(scenarios, DefaultConfig(), []int64{5_000, 5_500, 6_000})
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	if best.FalsePositives != 0 {
		t.Errorf("calibration chose a configuration listing the wash-traded market")
	}
}
