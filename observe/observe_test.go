package observe_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levu-lol/levu/observe"
)

var at = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func reg(t *testing.T) *observe.Registry {
	t.Helper()
	return observe.New(observe.DefaultThresholds()).WithClock(func() time.Time { return at })
}

func healthy() observe.LaneReport {
	return observe.LaneReport{
		Symbol: "CASHCAT", Quote: "USDG", MarketID: 1, State: "live",
		Seq: 5000, Epoch: 12, Intents: 5000, Fills: 900,
		StateSaves: 12, LeaseHeld: true, LeaseFence: 3,
		LastOracle: at.Add(-3 * time.Second), LastSettlement: at.Add(-2 * time.Minute),
	}
}

func names(as []observe.Alert) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Name
	}
	return out
}

func has(as []observe.Alert, name string) bool {
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	return false
}

func TestAHealthyLaneRaisesNothing(t *testing.T) {
	r := reg(t)
	r.Observe(healthy())
	if as := r.Alerts(); len(as) != 0 {
		t.Fatalf("a healthy lane alerted: %v", names(as))
	}
	if ok, _ := r.Healthy(); !ok {
		t.Fatal("a healthy lane reported unready")
	}
}

// The distinction the package exists for: a lane that has stopped looks like a
// quiet market from the outside. These are the signals that tell them apart.
func TestTheConditionsThatMeanALaneCannotProtectItself(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*observe.LaneReport)
		want   string
		sev    observe.Severity
	}{
		{"halted", func(l *observe.LaneReport) { l.Halted = true; l.HaltReason = "arithmetic failure" },
			"lane-halted", observe.Critical},
		{"insolvent", func(l *observe.LaneReport) { l.Insolvent = true },
			"lane-insolvent", observe.Critical},
		{"executing without a lease", func(l *observe.LaneReport) { l.LeaseHeld = false },
			"lease-lost", observe.Critical},
		{"stale oracle", func(l *observe.LaneReport) { l.LastOracle = at.Add(-10 * time.Minute) },
			"oracle-stale", observe.Critical},
		{"checkpoints failing", func(l *observe.LaneReport) { l.StateErrors = 4 },
			"checkpoints-failing", observe.Warning},
		{"never checkpointed", func(l *observe.LaneReport) { l.StateSaves = 0 },
			"never-checkpointed", observe.Warning},
		{"settlement backed up", func(l *observe.LaneReport) { l.LastSettlement = at.Add(-2 * time.Hour) },
			"settlement-stale", observe.Warning},
		{"bad debt", func(l *observe.LaneReport) { l.BadDebt = 2 },
			"bad-debt", observe.Warning},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := reg(t)
			l := healthy()
			c.mutate(&l)
			r.Observe(l)
			as := r.Alerts()
			if !has(as, c.want) {
				t.Fatalf("expected %q, got %v", c.want, names(as))
			}
			for _, a := range as {
				if a.Name == c.want {
					if a.Severity != c.sev {
						t.Errorf("%s is %s, want %s", c.want, a.Severity, c.sev)
					}
					if a.Detail == "" {
						t.Error("an alert with no detail makes whoever is paged start from nothing")
					}
					if a.Lane == "" {
						t.Error("alert does not say which lane")
					}
				}
			}
			ok, _ := r.Healthy()
			if c.sev == observe.Critical && ok {
				t.Error("a critical condition still reported ready")
			}
			if c.sev == observe.Warning && !ok {
				t.Error("a warning made the process unready; only critical should")
			}
		})
	}
}

// A halted lane already reports why it stopped; also shouting that it has no
// lease is noise on top of the thing that matters.
func TestAHaltedLaneDoesNotAlsoReportItsLease(t *testing.T) {
	r := reg(t)
	l := healthy()
	l.Halted, l.HaltReason, l.LeaseHeld = true, "lost the lease", false
	r.Observe(l)
	as := r.Alerts()
	if !has(as, "lane-halted") {
		t.Fatal("no halt alert")
	}
	if has(as, "lease-lost") {
		t.Error("a halted lane also raised lease-lost; that is duplicate noise")
	}
}

func TestAlertsAreOrderedWorstFirst(t *testing.T) {
	r := reg(t)
	l := healthy()
	l.BadDebt = 1                     // warning
	l.LastOracle = at.Add(-time.Hour) // critical
	r.Observe(l)
	as := r.Alerts()
	if len(as) < 2 {
		t.Fatalf("expected several alerts, got %v", names(as))
	}
	if as[0].Severity != observe.Critical {
		t.Fatalf("worst alert is not first: %v", names(as))
	}
}

func TestMetricsParseAndCarryTheLane(t *testing.T) {
	r := reg(t)
	r.Observe(healthy())
	r.Count("settlement-error")

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics returned %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`perps_lane_seq{lane="CASHCAT",quote="USDG",market_id="1",state="live"} 5000`,
		`perps_lane_halted{lane="CASHCAT",quote="USDG",market_id="1",state="live"} 0`,
		`perps_lane_lease_fence{lane="CASHCAT",quote="USDG",market_id="1",state="live"} 3`,
		`perps_events_total{event="settlement-error"} 1`,
		`perps_alerts{severity="critical"} 0`,
		"# TYPE perps_lane_seq counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing:\n  %s", want)
		}
	}

	// Every sample line must have a HELP and TYPE above it, or a scraper
	// rejects the page.
	var lastType string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			lastType = strings.Fields(line)[2]
		} else if line != "" && !strings.HasPrefix(line, "#") {
			name := line
			if i := strings.IndexAny(line, "{ "); i > 0 {
				name = line[:i]
			}
			if name != lastType {
				t.Errorf("sample %q has no preceding TYPE (last was %q)", name, lastType)
			}
		}
	}
}

// A lane symbol is attacker-influenced in a permissionless listing engine. A
// quote in one must not be able to break the metrics page for every other lane.
func TestAHostileSymbolCannotBreakTheMetricsPage(t *testing.T) {
	r := reg(t)
	l := healthy()
	l.Symbol = `evil","injected="1`
	r.Observe(l)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if strings.Contains(body, `injected="1"`) {
		t.Fatalf("label injection succeeded:\n%s", body)
	}
	if !strings.Contains(body, `\"`) {
		t.Error("the quote was not escaped at all")
	}
}

// Liveness must not fail on a business condition: a sequencer killed and
// restarted by its orchestrator has lost its lane to fix nothing.
func TestLivenessAndReadinessAreSeparate(t *testing.T) {
	r := reg(t)
	l := healthy()
	l.Halted, l.HaltReason = true, "arithmetic failure"
	r.Observe(l)
	h := r.Handler()

	live := httptest.NewRecorder()
	h.ServeHTTP(live, httptest.NewRequest("GET", "/healthz", nil))
	if live.Code != http.StatusOK {
		t.Errorf("liveness failed on a halted lane: %d", live.Code)
	}

	ready := httptest.NewRecorder()
	h.ServeHTTP(ready, httptest.NewRequest("GET", "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness passed with a halted lane: %d", ready.Code)
	}
	if !strings.Contains(ready.Body.String(), "arithmetic failure") {
		t.Errorf("readiness did not say why:\n%s", ready.Body.String())
	}
}

func TestALaneThatNeverHadAPriceIsNotReportedAsInfinitelyStale(t *testing.T) {
	r := reg(t)
	l := healthy()
	l.LastOracle = time.Time{}
	r.Observe(l)
	if has(r.Alerts(), "oracle-stale") {
		t.Error("a lane with no price yet was reported stale")
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "perps_lane_oracle_age_seconds{lane=\"CASHCAT\",quote=\"USDG\",market_id=\"1\",state=\"live\"} -1") {
		t.Error("never-observed should report -1, not a huge age")
	}
}
