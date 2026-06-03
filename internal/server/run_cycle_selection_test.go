package server

import "testing"

// TestSelectRunCycleForProjectionCanonicalOnly guards the graph route resolver:
// /runs/{run_number}/cycles/{cycle_number} resolves by canonical run-cycle
// address only. A stale ledger-form deep link (runs/9/cycles/1, where 9 is the
// cycle ledger of the run displaying as "6.1") must resolve to nothing rather
// than to that different run cycle.
func TestSelectRunCycleForProjectionCanonicalOnly(t *testing.T) {
	runs := []RunReport{
		{
			RunRef: "ambience#168/runs/4.1", RunNumber: intPtr(4),
			RunCycleNumber: intPtr(1), CycleNumber: intPtr(6), RunDisplayNumber: stringPtr("4.1"),
		},
		{
			RunRef: "ambience#168/runs/6.1", RunNumber: intPtr(6),
			RunCycleNumber: intPtr(1), CycleNumber: intPtr(9), RunDisplayNumber: stringPtr("6.1"),
		},
	}

	if run, ok := selectRunCycleForProjection(runs, "6", "1"); !ok || run.RunRef != "ambience#168/runs/6.1" {
		t.Fatalf("canonical 6/1 -> ok=%v ref=%q, want ambience#168/runs/6.1", ok, run.RunRef)
	}
	if run, ok := selectRunCycleForProjection(runs, "4", "1"); !ok || run.RunRef != "ambience#168/runs/4.1" {
		t.Fatalf("canonical 4/1 -> ok=%v ref=%q, want ambience#168/runs/4.1", ok, run.RunRef)
	}

	// Ledger-form and malformed segments must not resolve.
	for _, seg := range [][2]string{{"9", "1"}, {"6", "9"}, {"6", ""}, {"6.1", "1"}, {"0", "1"}, {"abc", "1"}} {
		if run, ok := selectRunCycleForProjection(runs, seg[0], seg[1]); ok {
			t.Fatalf("segments %q/%q must not resolve; got %q", seg[0], seg[1], run.RunRef)
		}
	}
}
