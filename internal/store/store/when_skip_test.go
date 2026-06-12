package store

import "testing"

func strPtr(s string) *string { return &s }

// A synthesized when-skip completion pre-satisfies the expected-job set, so
// the phase completes when the launched siblings finish.
func TestAllExpectedJobsCompletedCountsSynthesizedSkips(t *testing.T) {
	expected := []string{"test-plan", "implement"}
	completions := map[string]nativeJobCompletionDoc{
		"test-plan": {JobID: "test-plan", Conclusion: "skipped", CompletedAt: "2026-06-12T00:00:00Z"},
	}
	if allExpectedJobsCompleted(expected, completions) {
		t.Fatal("phase must wait for the launched sibling")
	}
	completions["implement"] = nativeJobCompletionDoc{JobID: "implement", Conclusion: "success", CompletedAt: "2026-06-12T00:01:00Z"}
	if !allExpectedJobsCompleted(expected, completions) {
		t.Fatal("phase must complete once launched jobs finish (skip pre-satisfied)")
	}
}

// A when-skipped job is verdict-neutral in the aggregated phase conclusion:
// it neither degrades a success nor masks a sibling failure.
func TestAggregateNativePhaseCompletionSkipNeutrality(t *testing.T) {
	expected := []string{"test-plan", "implement"}
	pass := aggregateNativePhaseCompletion(expected, map[string]nativeJobCompletionDoc{
		"test-plan": {JobID: "test-plan", Conclusion: "skipped", SummaryMarkdown: strPtr("job when condition: ...")},
		"implement": {JobID: "implement", Conclusion: "success", PhaseOutputs: map[string]string{"branch_name": "b"}},
	})
	if pass.Conclusion != "success" {
		t.Fatalf("skip must not degrade a succeeding phase, got %q", pass.Conclusion)
	}
	if pass.PhaseOutputs["branch_name"] != "b" {
		t.Fatalf("launched outputs must aggregate, got %v", pass.PhaseOutputs)
	}
	fail := aggregateNativePhaseCompletion(expected, map[string]nativeJobCompletionDoc{
		"test-plan": {JobID: "test-plan", Conclusion: "skipped"},
		"implement": {JobID: "implement", Conclusion: "failure"},
	})
	if fail.Conclusion != "failure" {
		t.Fatalf("skip must not mask a sibling failure, got %q", fail.Conclusion)
	}
}

func TestAnySkippedJobCompletion(t *testing.T) {
	if anySkippedJobCompletion(map[string]nativeJobCompletionDoc{"a": {Conclusion: "success"}}) {
		t.Fatal("success-only completions must not read as skipped")
	}
	if !anySkippedJobCompletion(map[string]nativeJobCompletionDoc{"a": {Conclusion: "skipped"}}) {
		t.Fatal("a skipped completion must be detected")
	}
}
