package phaserefs

import "testing"

// Outputs a when-skipped leg would have published resolve to empty strings;
// phases with no skips keep the strict fail-closed behavior.
func TestSubstituteResolvesSkippedPhaseOutputsEmpty(t *testing.T) {
	phase := Phase{
		Name:   "verify",
		Inputs: map[string]string{"test_plan": "${{ phases.work.outputs.test_plan }}", "branch": "${{ phases.work.outputs.branch_name }}"},
	}
	prior := map[string]map[string]string{"work": {"branch_name": "b"}}

	if _, err := Substitute(phase, prior, nil); err == nil {
		t.Fatal("missing output without a skip must fail closed")
	}

	resolved, err := Substitute(phase, prior, map[string]bool{"work": true})
	if err != nil {
		t.Fatalf("skipped-phase substitution must not error: %v", err)
	}
	if resolved["test_plan"] != "" || resolved["branch"] != "b" {
		t.Fatalf("skipped output must resolve empty, published output must resolve, got %v", resolved)
	}

	// A wholly skipped phase (no captured outputs at all) resolves empty too.
	resolved, err = Substitute(Phase{Name: "x", Inputs: map[string]string{"v": "${{ phases.gone.outputs.k }}"}}, map[string]map[string]string{}, map[string]bool{"gone": true})
	if err != nil || resolved["v"] != "" {
		t.Fatalf("wholly skipped phase must resolve empty, got %v err=%v", resolved, err)
	}
}
