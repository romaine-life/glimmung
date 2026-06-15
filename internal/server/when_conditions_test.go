package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newCompletionRecorderFor(t *testing.T, store *fakeCompletionStore, launcher *fakeRunLauncher, payload RunnerCompletedRequest) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	newCompletionHandler(store, launcher).ServeHTTP(rec, runnerCompletionRequest("tok", payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec
}

// --- registration validation ---

func whenTestRegister() WorkflowRegister {
	return WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Vars:    map[string]string{"feature_type": "effect"},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "work", DependsOn: []string{"prepare"}, Jobs: []RunnerJobSpec{
				{ID: "test-plan", When: "${{ vars.feature_type }} != 'effect'"},
				{ID: "implement"},
			}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"work"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: PRReviewJobID, Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}},
		},
	}
}

func TestValidateWorkflowRegisterAcceptsWhenAndVars(t *testing.T) {
	if err := ValidateWorkflowRegister(whenTestRegister()); err != nil {
		t.Fatalf("valid when/vars registration rejected: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsRetiredSkipWhenPreserveTestEnv(t *testing.T) {
	// Migration guard: the retired field must stay rejected so the old
	// path cannot be reintroduced into live registrations.
	req := whenTestRegister()
	req.Phases[3].When = ""
	req.Phases[3].SkipWhenPreserveTestEnv = true
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "retired field skip_when_preserve_test_env") || !strings.Contains(err.Error(), "run.preserve_test_env") {
		t.Fatalf("retired field must be rejected pointing at the when replacement, got: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsWhenOnVerifyPhaseAndJobs(t *testing.T) {
	req := whenTestRegister()
	req.Phases[2].When = "${{ vars.feature_type }} == 'effect'"
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "verification and review-gate phases always run") {
		t.Fatalf("when on verify phase must be rejected, got: %v", err)
	}

	req = whenTestRegister()
	req.Phases[2].Jobs[0].When = "${{ vars.feature_type }} == 'effect'"
	err = ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "always run") {
		t.Fatalf("when on verify job must be rejected, got: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsWhenOnEntryPhase(t *testing.T) {
	req := whenTestRegister()
	req.Phases[0].When = "${{ vars.feature_type }} == 'effect'"
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "entry phase") {
		t.Fatalf("when on entry phase must be rejected, got: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsAllJobsConditional(t *testing.T) {
	req := whenTestRegister()
	req.Phases[1].Jobs[1].When = "${{ vars.feature_type }} == 'effect'"
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "at least one job must be unconditional") {
		t.Fatalf("all-conditional phase must be rejected, got: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsUndeclaredVarsRefInWhen(t *testing.T) {
	req := whenTestRegister()
	req.Phases[1].Jobs[0].When = "${{ vars.nope }} != 'effect'"
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "vars map does not declare \"nope\"") {
		t.Fatalf("undeclared vars ref must be rejected with the key named, got: %v", err)
	}
}

func TestValidateWorkflowRegisterRejectsInvalidVarsKey(t *testing.T) {
	req := whenTestRegister()
	req.Vars["bad key"] = "x"
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "vars key") {
		t.Fatalf("invalid vars key must be rejected, got: %v", err)
	}
}

// --- dispatch behavior ---

// conditionalWorkflowForCompletion declares impl -> work2 where work2 has a
// conditional test-plan-style job next to an unconditional implement-style
// job, plus the trailing teardown the shape requires.
func conditionalWorkflowForCompletion(vars map[string]string, testPlanWhen, phaseWhen string) *Workflow {
	wf := &Workflow{
		Project: "proj",
		Name:    "wf",
		Vars:    vars,
		Phases: []PhaseSpec{
			{Name: "impl", Kind: "k8s_job", Outputs: []string{"branch_name"}, Jobs: []RunnerJobSpec{{ID: "impl-job"}}},
			{Name: "work2", Kind: "k8s_job", When: phaseWhen, DependsOn: []string{"impl"}, Jobs: []RunnerJobSpec{
				{ID: "test-plan", When: testPlanWhen, Steps: []RunnerStepSpec{{Slug: "emit", Type: "run", Run: "true"}}},
				{ID: "implement", Steps: []RunnerStepSpec{{Slug: "emit", Type: "run", Run: "true"}}},
			}},
			{Name: "cleanup", Kind: "k8s_job", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"work2"}, Jobs: []RunnerJobSpec{{ID: "cleanup-job"}}},
		},
	}
	canonical := CanonicalWorkflow(*wf)
	return &canonical
}

func TestForwardDispatchSkipsConditionalJobWithoutLaunchingIt(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts = []RunAttemptData{{AttemptIndex: 0, Phase: "impl", Conclusion: "failure"}}
	store.wf = conditionalWorkflowForCompletion(map[string]string{"feature_type": "effect"}, "${{ vars.feature_type }} != 'effect'", "")
	store.leaseResult = Lease{State: "claimed"}
	launcher := &fakeRunLauncher{}

	rec := newCompletionRecorderFor(t, store, launcher, completedJob("impl-job", "success", nil, map[string]string{"branch_name": "b"}))

	if store.appendPhase != "work2" {
		t.Fatalf("appended phase=%q body=%s", store.appendPhase, rec.Body.String())
	}
	if store.skippedJobsPhase != "work2" {
		t.Fatalf("skipped jobs recorded on phase=%q, want work2", store.skippedJobsPhase)
	}
	trace, ok := store.skippedJobs["test-plan"]
	if !ok {
		t.Fatalf("test-plan must be recorded skipped, got %v", store.skippedJobs)
	}
	for _, fragment := range []string{"vars.feature_type", "'effect'", "false"} {
		if !strings.Contains(trace, fragment) {
			t.Fatalf("skip trace %q must carry the resolution (%q missing)", trace, fragment)
		}
	}
	if !launcher.called {
		t.Fatal("launcher must be called for the launched sibling")
	}
	if _, ok := launcher.req.SkipJobIDs["test-plan"]; !ok {
		t.Fatalf("launcher must receive SkipJobIDs for test-plan, got %v", launcher.req.SkipJobIDs)
	}
	if _, ok := launcher.req.SkipJobIDs["implement"]; ok {
		t.Fatal("unconditional job must not be in SkipJobIDs")
	}
	if store.skippedStampCalls != 0 {
		t.Fatalf("partial job skip must not stamp the whole phase skipped (calls=%d)", store.skippedStampCalls)
	}
}

func TestForwardDispatchStampsPhaseSkippedOnPhaseWhen(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts = []RunAttemptData{{AttemptIndex: 0, Phase: "impl", Conclusion: "failure"}}
	store.wf = conditionalWorkflowForCompletion(map[string]string{"feature_type": "effect"}, "", "${{ vars.feature_type }} != 'effect'")
	store.leaseResult = Lease{State: "claimed"}
	launcher := &fakeRunLauncher{}

	_ = newCompletionRecorderFor(t, store, launcher, completedJob("impl-job", "success", nil, map[string]string{"branch_name": "b"}))

	if store.skippedStampCalls != 1 {
		t.Fatalf("phase when=false must stamp a skipped attempt (calls=%d)", store.skippedStampCalls)
	}
	for _, fragment := range []string{"phase when condition", "vars.feature_type"} {
		if !strings.Contains(store.skippedStampReason, fragment) {
			t.Fatalf("skip reason %q must carry the resolved condition (%q missing)", store.skippedStampReason, fragment)
		}
	}
	if launcher.called && launcher.req.Phase.Name == "work2" {
		t.Fatal("a phase skipped by when must not launch its jobs")
	}
}

func TestForwardDispatchRunsConditionalJobWhenConditionHolds(t *testing.T) {
	store := &fakeCompletionStore{tokenRunID: "r1", tokenProject: "proj"}
	store.run = runDataForCompletion("impl")
	store.run.Attempts = []RunAttemptData{{AttemptIndex: 0, Phase: "impl", Conclusion: "failure"}}
	store.wf = conditionalWorkflowForCompletion(map[string]string{"feature_type": "stats-display"}, "${{ vars.feature_type }} != 'effect'", "")
	store.leaseResult = Lease{State: "claimed"}
	launcher := &fakeRunLauncher{}

	_ = newCompletionRecorderFor(t, store, launcher, completedJob("impl-job", "success", nil, map[string]string{"branch_name": "b"}))

	if len(store.skippedJobs) != 0 {
		t.Fatalf("no jobs should skip when the condition holds, got %v", store.skippedJobs)
	}
	if len(launcher.req.SkipJobIDs) != 0 {
		t.Fatalf("launcher must receive no SkipJobIDs, got %v", launcher.req.SkipJobIDs)
	}
}
