package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Vars must survive every Workflow -> WorkflowRegister reconstruction on the
// dispatch path: a registered conditional shape (vars + when) that validates
// at registration must also validate at dispatch-time re-validation, or the
// workflow is registrable but never runnable (the ambience#167 run-13
// dispatch 422). The fixture skips one prepare job by condition and asserts
// the launcher receives the skip — proving both the round-trip and the
// entry-phase job-level evaluation.
func TestDispatchRunRoundTripsVarsAndSkipsConditionalEntryJob(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.Vars = map[string]string{"issue_contract": "off"}
	for i := range store.wf.Phases {
		if store.wf.Phases[i].Name != "prepare" {
			continue
		}
		store.wf.Phases[i].Jobs = []NativeJobSpec{
			{ID: "env-prep", Steps: []NativeStepSpec{{Slug: "run", Type: "run", Run: "true"}}},
			{ID: "issue-contract", When: "${{ vars.issue_contract }} == 'on'", Steps: []NativeStepSpec{{Slug: "run", Type: "run", Run: "true"}}},
		}
	}
	// resolveDispatchWorkflow takes the ListProjectWorkflows path for a
	// nameless dispatch; keep that copy in sync with the mutated fixture.
	store.workflows = []Workflow{*store.wf}
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s (vars dropped on a dispatch-path reconstruction?)", rec.Code, rec.Body.String())
	}
	if result := readDispatchResult(t, rec); result.State != "dispatched" {
		t.Fatalf("state=%q", result.State)
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	trace, ok := launcher.req.SkipJobIDs["issue-contract"]
	if !ok {
		t.Fatalf("conditional entry job must be skipped, SkipJobIDs=%v", launcher.req.SkipJobIDs)
	}
	if !strings.Contains(trace, "vars.issue_contract") {
		t.Fatalf("skip trace %q must carry the resolved condition", trace)
	}
	if store.skippedJobsPhase != "prepare" || store.skippedJobs["issue-contract"] == "" {
		t.Fatalf("synthesized skip must be recorded on prepare: phase=%q jobs=%v", store.skippedJobsPhase, store.skippedJobs)
	}
}
