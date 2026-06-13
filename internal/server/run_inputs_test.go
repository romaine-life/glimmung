package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ValidateRunInputs is the structural backstop store.CreateRun enforces. These
// lock in the invariant: a run's inputs must cover exactly the workflow's
// declared dispatch_inputs.
func TestValidateRunInputs(t *testing.T) {
	declared := []DispatchInputSpec{{Name: "git_ref", Required: true, Default: "main"}}

	if err := ValidateRunInputs(declared, map[string]string{"git_ref": "main"}); err != nil {
		t.Fatalf("satisfied inputs rejected: %v", err)
	}
	if err := ValidateRunInputs(nil, map[string]string{}); err != nil {
		t.Fatalf("empty inputs with no declared inputs rejected: %v", err)
	}
	if err := ValidateRunInputs(declared, map[string]string{}); err == nil || !strings.Contains(err.Error(), "git_ref") {
		t.Fatalf("missing declared input not rejected: %v", err)
	}
	if err := ValidateRunInputs(declared, nil); err == nil {
		t.Fatalf("nil inputs against a declared input not rejected")
	}
	if err := ValidateRunInputs(nil, map[string]string{"git_ref": "main"}); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared input not rejected: %v", err)
	}
}

// Regression for the stranded-run bug: synthetic dispatch must resolve the
// workflow's declared dispatch_inputs (applying defaults like git_ref -> main)
// so the created run's RunInputs satisfy `${{ inputs.git_ref }}` phase
// templates — rather than creating the run + claiming a lease and then failing
// at phase-dispatch render with "input \"git_ref\" ... was not provided".
func TestSyntheticDispatchResolvesDeclaredGitRefDefault(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.DispatchInputs = []DispatchInputSpec{{Name: "git_ref", Required: true, Default: "main"}}
	store.workflows[0].DispatchInputs = store.wf.DispatchInputs
	store.wf.Phases[1].Inputs = map[string]string{
		"issue_contract": "${{ phases.prepare.outputs.issue_contract }}",
	}
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "verify",
		Reason:       "break-glass retry verify",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{{
			Phase:        "prepare",
			PhaseOutputs: map[string]string{"issue_contract": `{"target":"portal"}`},
		}},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, launcher).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq == nil {
		t.Fatal("CreateRun was not called")
	}
	if got := store.runReq.RunInputs["git_ref"]; got != "main" {
		t.Fatalf("CreateRun RunInputs[git_ref]=%q, want \"main\" (declared default applied); full=%#v", got, store.runReq.RunInputs)
	}
}

// A required input with no default cannot be satisfied by synthetic dispatch
// (it carries no caller inputs), so it must be rejected up front — 422, no run
// row, no lease/lock — instead of stranding a dispatch_failed run.
func TestSyntheticDispatchRejectsUnsatisfiableRequiredInput(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.DispatchInputs = []DispatchInputSpec{{Name: "git_ref", Required: true}}
	store.workflows[0].DispatchInputs = store.wf.DispatchInputs
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "verify",
		Reason:       "break-glass retry verify",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{{
			Phase:        "prepare",
			PhaseOutputs: map[string]string{"issue_contract": `{"target":"portal"}`},
		}},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", rec.Code, rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun must not run when a required input is unsatisfiable: %#v", store.runReq)
	}
}
