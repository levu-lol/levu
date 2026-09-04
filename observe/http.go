package observe

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Handler serves the three endpoints an operator and a scraper need.
//
//	/metrics  Prometheus text format
//	/healthz  liveness: is this process running
//	/readyz   readiness: is anything critically wrong, with the reasons
//
// Split deliberately. Liveness that fails on a business condition gets the
// process killed and restarted by the orchestrator, which for a sequencer means
// losing the lane to solve a problem a restart cannot fix. Liveness answers
// "am I here"; readiness answers "should you rely on me".
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", r.serveMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", r.serveReady)
	mux.HandleFunc("/alerts", r.serveAlerts)
	return mux
}

func (r *Registry) serveReady(w http.ResponseWriter, _ *http.Request) {
	ok, alerts := r.Healthy()
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	// The reasons go in the body either way. A readiness probe that says only
	// "no" makes whoever is paged start the investigation from nothing.
	for _, a := range alerts {
		fmt.Fprintln(w, a)
	}
	if len(alerts) == 0 {
		fmt.Fprintln(w, "ok")
	}
}

func (r *Registry) serveAlerts(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	alerts := r.Alerts()
	type wire struct {
		Severity string `json:"severity"`
		Lane     string `json:"lane,omitempty"`
		Name     string `json:"name"`
		Detail   string `json:"detail"`
	}
	out := make([]wire, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, wire{a.Severity.String(), a.Lane, a.Name, a.Detail})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// escape handles the label-value rules of the exposition format. A symbol with
// a quote or a backslash in it would otherwise produce a metrics page that
// fails to parse, taking every other metric down with it.
func escape(s string) string {
	rep := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return rep.Replace(s)
}

func (r *Registry) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	r.mu.RLock()
	lanes := make([]LaneReport, 0, len(r.lanes))
	for _, l := range r.lanes {
		lanes = append(lanes, l)
	}
	counts := make(map[string]uint64, len(r.counts))
	for k, v := range r.counts {
		counts[k] = v
	}
	now := r.now()
	start := r.start
	r.mu.RUnlock()

	sort.Slice(lanes, func(i, j int) bool { return lanes[i].MarketID < lanes[j].MarketID })

	w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }

	p("# HELP perps_uptime_seconds Seconds since this process started.")
	p("# TYPE perps_uptime_seconds gauge")
	p("perps_uptime_seconds %d", int64(now.Sub(start).Seconds()))

	type metric struct {
		name, help, typ string
		get             func(LaneReport) float64
	}
	metrics := []metric{
		{"perps_lane_seq", "Lane sequence number.", "counter", func(l LaneReport) float64 { return float64(l.Seq) }},
		{"perps_lane_epoch", "Lane epoch.", "counter", func(l LaneReport) float64 { return float64(l.Epoch) }},
		{"perps_lane_intents_total", "Intents applied.", "counter", func(l LaneReport) float64 { return float64(l.Intents) }},
		{"perps_lane_fills_total", "Fills produced.", "counter", func(l LaneReport) float64 { return float64(l.Fills) }},
		{"perps_lane_liquidations_total", "Liquidations executed.", "counter", func(l LaneReport) float64 { return float64(l.Liquidations) }},
		{"perps_lane_settlements_total", "Roots committed on chain.", "counter", func(l LaneReport) float64 { return float64(l.Settlements) }},
		{"perps_lane_bad_debt_events_total", "Losses the insurance fund had to absorb.", "counter", func(l LaneReport) float64 { return float64(l.BadDebt) }},
		{"perps_lane_state_saves_total", "Recovery checkpoints written.", "counter", func(l LaneReport) float64 { return float64(l.StateSaves) }},
		{"perps_lane_state_errors_total", "Recovery checkpoints that failed.", "counter", func(l LaneReport) float64 { return float64(l.StateErrors) }},
		{"perps_lane_halted", "1 if the lane has stopped executing.", "gauge", func(l LaneReport) float64 { return b2f(l.Halted) }},
		{"perps_lane_insolvent", "1 if insurance did not cover realised losses.", "gauge", func(l LaneReport) float64 { return b2f(l.Insolvent) }},
		{"perps_lane_lease_held", "1 if this process may still execute the lane.", "gauge", func(l LaneReport) float64 { return b2f(l.LeaseHeld) }},
		{"perps_lane_lease_fence", "Fencing token of the held lease.", "gauge", func(l LaneReport) float64 { return float64(l.LeaseFence) }},
		{"perps_lane_oracle_age_seconds", "Seconds since this lane last saw a price.", "gauge", func(l LaneReport) float64 { return age(now, l.LastOracle) }},
		{"perps_lane_settlement_age_seconds", "Seconds since the last accepted commitment.", "gauge", func(l LaneReport) float64 { return age(now, l.LastSettlement) }},
	}
	for _, m := range metrics {
		p("# HELP %s %s", m.name, m.help)
		p("# TYPE %s %s", m.name, m.typ)
		for _, l := range lanes {
			p(`%s{lane="%s",quote="%s",market_id="%d",state="%s"} %g`,
				m.name, escape(l.Symbol), escape(l.Quote), l.MarketID, escape(l.State), m.get(l))
		}
	}

	// Alerts are exported as a metric too, so paging can be driven from the
	// same scrape rather than from a second mechanism that can disagree.
	p("# HELP perps_alerts Active alerts by severity.")
	p("# TYPE perps_alerts gauge")
	bySeverity := map[Severity]int{Info: 0, Warning: 0, Critical: 0}
	for _, a := range r.Alerts() {
		bySeverity[a.Severity]++
	}
	for _, s := range []Severity{Info, Warning, Critical} {
		p(`perps_alerts{severity="%s"} %d`, s, bySeverity[s])
	}

	if len(counts) > 0 {
		names := make([]string, 0, len(counts))
		for k := range counts {
			names = append(names, k)
		}
		sort.Strings(names)
		p("# HELP perps_events_total Free-form process events.")
		p("# TYPE perps_events_total counter")
		for _, n := range names {
			p(`perps_events_total{event="%s"} %d`, escape(n), counts[n])
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// age reports -1 rather than a huge number for "never", so a dashboard does not
// show a lane that has never had a price as the most stale thing on the page.
func age(now, t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	return now.Sub(t).Seconds()
}
