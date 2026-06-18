package store

import (
	"testing"

	"github.com/romaine-life/glimmung/internal/metrics"
	"github.com/romaine-life/glimmung/internal/server"
)

// runTerminalCounter reads the current value of
// glimmung_run_terminal_total{class,state} straight from the package registry.
// Returns 0 when the labelled series has not been touched yet.
func runTerminalCounter(t *testing.T, class, state string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != "glimmung_run_terminal_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			gotClass, gotState := "", ""
			for _, lbl := range m.GetLabel() {
				switch lbl.GetName() {
				case "class":
					gotClass = lbl.GetValue()
				case "state":
					gotState = lbl.GetValue()
				}
			}
			if gotClass == class && gotState == state {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// recordTerminalSettle is the single emission point shared by the genuine
// terminal-write choke points. This proves it increments the backstop counter
// exactly once per call, labels by the guarded class + state, uses the "none"
// sentinel for a passed run (no observation), and does not double-count.
func TestRecordTerminalSettleLabelsAndSingleIncrement(t *testing.T) {
	s := &Store{}
	doc := runDoc{ID: "run-xyz", Project: "demo", IssueNumber: 7}

	cases := []struct {
		name  string
		state string
		obs   *server.RunTerminalObservation
		class string
	}{
		{
			name:  "verifier_failed abort",
			state: "aborted",
			obs:   &server.RunTerminalObservation{Class: server.TerminalObservationVerifierFailed, Phase: "llm-verify", JobID: "llm-verify"},
			class: server.TerminalObservationVerifierFailed,
		},
		{
			name:  "malformed_terminal abort (the alert signal)",
			state: "aborted",
			obs:   &server.RunTerminalObservation{Class: server.TerminalObservationMalformed, Message: "MALFORMED TERMINAL: ..."},
			class: server.TerminalObservationMalformed,
		},
		{
			name:  "passed run uses none sentinel",
			state: "passed",
			obs:   nil,
			class: server.TerminalObservationClassNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := runTerminalCounter(t, tc.class, tc.state)
			s.recordTerminalSettle(doc, "demo#7.1", tc.state, tc.obs)
			after := runTerminalCounter(t, tc.class, tc.state)
			if after-before != 1 {
				t.Fatalf("run_terminal_total{class=%s,state=%s}: expected +1, got +%v", tc.class, tc.state, after-before)
			}
		})
	}
}

// A single settle must move the counter exactly once — the no-double-count
// guarantee that keeps Repair (a re-derivation) from inflating the metric.
func TestRecordTerminalSettleDoesNotDoubleCount(t *testing.T) {
	s := &Store{}
	doc := runDoc{ID: "run-once", Project: "demo", IssueNumber: 9}
	obs := &server.RunTerminalObservation{Class: server.TerminalObservationManualAbort, Reason: "operator stopped the run"}

	before := runTerminalCounter(t, server.TerminalObservationManualAbort, "aborted")
	s.recordTerminalSettle(doc, "demo#9.1", "aborted", obs)
	after := runTerminalCounter(t, server.TerminalObservationManualAbort, "aborted")
	if after-before != 1 {
		t.Fatalf("a single settle must increment exactly once: got +%v", after-before)
	}
}

func TestTerminalSettleCyclePrefersRunCycleNumber(t *testing.T) {
	rc, cyc := 3, 5
	if got := terminalSettleCycle(runDoc{RunCycleNumber: &rc, CycleNumber: &cyc}); got != 3 {
		t.Fatalf("want run_cycle_number 3, got %d", got)
	}
	if got := terminalSettleCycle(runDoc{CycleNumber: &cyc}); got != 5 {
		t.Fatalf("want cycle_number fallback 5, got %d", got)
	}
	if got := terminalSettleCycle(runDoc{}); got != 0 {
		t.Fatalf("want 0 for non-recycle, got %d", got)
	}
}
