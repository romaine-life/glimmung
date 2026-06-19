package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/auth"
)

type fakeSyntheticCopyDispatchStore struct {
	*fakeDispatchStore
	sourceRunID  string
	sourceRunRef string
	sourceRun    RunReplayData
}

func (s *fakeSyntheticCopyDispatchStore) ReadRunIDForNumber(_ context.Context, _ string, _ int, runNumber string) (string, string, error) {
	if runNumber != "17.1" {
		return "", "", ErrNotFound
	}
	return s.sourceRunID, s.sourceRunRef, nil
}

func (s *fakeSyntheticCopyDispatchStore) ReadRunForReplay(_ context.Context, _ string, runID string) (RunReplayData, error) {
	if runID != s.sourceRunID {
		return RunReplayData{}, ErrNotFound
	}
	return s.sourceRun, nil
}

func newSyntheticDispatchTestHandler(store ReadStore, runLauncher RunLauncher) http.Handler {
	adminAuthenticator := fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/runs/synthetic-dispatch", requireAdmin(adminAuthenticator, http.HandlerFunc(syntheticDispatchRunHandler(Settings{}, store, runLauncher))))
	return mux
}

func TestSyntheticDispatchStartsAtRequestedPhaseWithSuppliedOutputs(t *testing.T) {
	store := minimalDispatchStore()
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
		ExecutionContext: SyntheticExecutionContext{
			SlotLeaseRef:  "lease-1",
			Namespace:     "proj-slot-1",
			ValidationURL: "https://slot.example",
		},
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
	if store.runReq.EntrypointPhase != "verify" {
		t.Fatalf("entrypoint=%q, want verify", store.runReq.EntrypointPhase)
	}
	if len(store.runReq.SuppliedAttempts) != 1 || store.runReq.SuppliedAttempts[0].Phase != "prepare" || store.runReq.SuppliedAttempts[0].Conclusion != "supplied" {
		t.Fatalf("supplied attempts=%#v", store.runReq.SuppliedAttempts)
	}
	if store.startReq == nil || store.startReq.PhaseName != "verify" || store.startReq.SlotLeaseRef != "lease-1" {
		t.Fatalf("start request=%#v", store.startReq)
	}
	if !launcher.called || launcher.req.Phase.Name != "verify" {
		t.Fatalf("launcher request=%#v", launcher.req)
	}
	phaseInputs, ok := launcher.req.Lease.Metadata["phase_inputs"].(map[string]string)
	if !ok || phaseInputs["issue_contract"] != `{"target":"portal"}` {
		t.Fatalf("phase_inputs=%#v", launcher.req.Lease.Metadata["phase_inputs"])
	}
	if got := launcher.req.Lease.Metadata["synthetic_namespace"]; got != "proj-slot-1" {
		t.Fatalf("synthetic namespace=%#v", got)
	}
}

func TestSyntheticDispatchStartsAtReviewSlotlessWithoutLease(t *testing.T) {
	// A synthetic run whose start->terminal span is review/review_gate/teardown
	// touches no test environment, so it dispatches with NO claimed lease: the
	// host-free, produce-free evidence/review replay. Supply the verify verdict +
	// recorded evidence so review replays exactly what a prior run produced.
	store := minimalDispatchStore()
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "review",
		Reason:       "slotless evidence replay into review",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{
			{
				Phase: "verify",
				Verification: &RunVerificationData{
					Status:       "pass",
					EvidenceRefs: []string{"runs/proj/run-1/screenshots/tooltip.png"},
				},
			},
			{Phase: "cleanup_early", PhaseOutputs: map[string]string{}},
		},
		// No SlotLeaseRef: review/review_gate/cleanup need no test slot.
		ExecutionContext: SyntheticExecutionContext{},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, launcher).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq == nil || store.runReq.EntrypointPhase != "review" {
		t.Fatalf("run req=%#v", store.runReq)
	}
	if store.runReq.SlotLeaseRef != "" {
		t.Fatalf("slotless run must carry no slot lease ref, got %q", store.runReq.SlotLeaseRef)
	}
	if store.startReq == nil || store.startReq.SlotLeaseRef != "" {
		t.Fatalf("start req must be slotless, got %#v", store.startReq)
	}
	if !launcher.called || launcher.req.Phase.Name != "review" {
		t.Fatalf("launcher req=%#v", launcher.req)
	}
	// The phase Job launches with a zero-value lease; its context comes from the run record.
	if launcher.req.Lease.State != "" {
		t.Fatalf("slotless launch must use a zero-value lease, got state=%q", launcher.req.Lease.State)
	}
}

func TestSyntheticDispatchRequiresLeaseForEnvironmentPhaseStart(t *testing.T) {
	// Starting at an environment phase (verify) without a claimed lease is a 422:
	// the run will execute on the test slot, so a lease is mandatory. The check
	// fires before any launch.
	store := minimalDispatchStore()
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "verify",
		Reason:           "verify needs the slot",
		ExecutionContext: SyntheticExecutionContext{}, // no lease
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, launcher).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "slot_lease_ref required") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if launcher.called {
		t.Fatal("environment-phase start without a lease must not launch")
	}
}

func TestSyntheticDispatchCopiesSelectedPhaseOutputsFromPriorRun(t *testing.T) {
	base := minimalDispatchStore()
	verification := `{"status":"pass","evidence_refs":["/v1/artifacts/runs/proj/run-17/videos/portal.webm"]}`
	store := &fakeSyntheticCopyDispatchStore{
		fakeDispatchStore: base,
		sourceRunID:       "run-17",
		sourceRunRef:      "proj#7/runs/17.1",
		sourceRun: RunReplayData{
			ID:      "run-17",
			Project: "proj",
			Attempts: []RunAttemptData{{
				AttemptIndex: 2,
				Phase:        "verify",
				Completed:    true,
				Decision:     "advance",
				PhaseOutputs: map[string]string{"verification": verification, "other": "ignored"},
			}},
		},
	}
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "review",
		Reason:       "retry review without rerunning verifier",
		CopyPhaseOutputsFrom: &SyntheticCopyPhaseOutputsFrom{
			Run:    "17.1",
			Phases: map[string][]string{"verify": []string{"verification"}},
		},
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{{
			Phase:        "cleanup_early",
			PhaseOutputs: map[string]string{},
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
	if len(store.runReq.SuppliedAttempts) != 2 {
		t.Fatalf("supplied attempts=%#v", store.runReq.SuppliedAttempts)
	}
	verify := store.runReq.SuppliedAttempts[0]
	if verify.Phase != "verify" || verify.PhaseOutputs["verification"] != verification {
		t.Fatalf("copied verify attempt=%#v", verify)
	}
	if verify.Verification != nil {
		t.Fatalf("verification=%#v, want nil when only phase output was copied", verify.Verification)
	}
	provenance, ok := store.runReq.TriggerSource["copied_phase_outputs"].([]map[string]any)
	if !ok || len(provenance) != 1 {
		t.Fatalf("provenance=%#v", store.runReq.TriggerSource["copied_phase_outputs"])
	}
	if provenance[0]["source_run"] != "proj#7/runs/17.1" || provenance[0]["source_phase"] != "verify" {
		t.Fatalf("provenance=%#v", provenance[0])
	}
}

func TestSyntheticDispatchSuppliesTypedVerificationEvidence(t *testing.T) {
	store := minimalDispatchStore()
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "review",
		Reason:       "retry review with recovered verifier evidence",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{
			{
				Phase:        "prepare",
				PhaseOutputs: map[string]string{"issue_contract": `{"target":"portal"}`},
			},
			{
				Phase: "verify",
				Verification: &RunVerificationData{
					Status:       "pass",
					Reasons:      []string{"tooltip showed Energy generated 1"},
					EvidenceRefs: []string{"runs/proj/run-1/screenshots/happy-flower.png"},
					Evidence: []EvidenceArtifact{{
						Kind:  EvidenceKindScreenshot,
						Ref:   "runs/proj/run-1/screenshots/happy-flower.png",
						Label: "Happy Flower tooltip",
					}},
				},
			},
			{
				Phase:        "cleanup_early",
				PhaseOutputs: map[string]string{},
			},
		},
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
	if len(store.runReq.SuppliedAttempts) != 3 {
		t.Fatalf("supplied attempts=%#v", store.runReq.SuppliedAttempts)
	}
	verify := store.runReq.SuppliedAttempts[1]
	if verify.Phase != "verify" || verify.Conclusion != "success" || verify.Decision != "advance" || !verify.CarryForward {
		t.Fatalf("verify attempt=%#v", verify)
	}
	if verify.Verification == nil || verify.Verification.Status != "pass" {
		t.Fatalf("verification=%#v", verify.Verification)
	}
	if got := verify.Verification.EvidenceRefs; len(got) != 1 || got[0] != "runs/proj/run-1/screenshots/happy-flower.png" {
		t.Fatalf("evidence_refs=%#v", got)
	}
	if got := verify.Verification.Evidence; len(got) != 1 || got[0].Kind != EvidenceKindScreenshot || got[0].Label != "Happy Flower tooltip" {
		t.Fatalf("evidence=%#v", got)
	}
	launchVerify := launcher.req.Run.Attempts[1]
	if launchVerify.Verification == nil || launchVerify.Verification.EvidenceRefs[0] != "runs/proj/run-1/screenshots/happy-flower.png" {
		t.Fatalf("launcher attempts=%#v", launcher.req.Run.Attempts)
	}
}

func TestSyntheticDispatchRejectsVerificationOnNonVerifyPhase(t *testing.T) {
	store := minimalDispatchStore()
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "verify",
		Reason:       "bad supplied verification",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{{
			Phase:        "prepare",
			PhaseOutputs: map[string]string{"issue_contract": `{"target":"portal"}`},
			Verification: &RunVerificationData{Status: "pass"},
		}},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot include verification") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}

func TestSyntheticDispatchRejectsNonPassingSuppliedVerification(t *testing.T) {
	store := minimalDispatchStore()
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "review",
		Reason:       "bad supplied verification",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{
			{
				Phase:        "prepare",
				PhaseOutputs: map[string]string{"issue_contract": `{"target":"portal"}`},
			},
			{
				Phase:        "verify",
				Verification: &RunVerificationData{Status: "fail", Reasons: []string{"still broken"}},
			},
			{
				Phase:        "cleanup_early",
				PhaseOutputs: map[string]string{},
			},
		},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verification.status must be") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}

func TestSyntheticDispatchRejectsCopiedPhaseAtOrAfterStart(t *testing.T) {
	base := minimalDispatchStore()
	store := &fakeSyntheticCopyDispatchStore{
		fakeDispatchStore: base,
		sourceRunID:       "run-17",
		sourceRunRef:      "proj#7/runs/17.1",
		sourceRun:         RunReplayData{ID: "run-17"},
	}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "review",
		Reason:       "bad copy",
		CopyPhaseOutputsFrom: &SyntheticCopyPhaseOutputsFrom{
			Run:    "17.1",
			Phases: map[string][]string{"review": []string{"pr_url"}},
		},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must be before start_at_phase") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}

func TestSyntheticDispatchRejectsStartAfterManagedPRReviewPhase(t *testing.T) {
	store := minimalDispatchStore()
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "review_gate",
		Reason:           "bad synthetic gate start",
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot start after managed PR review phase") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}

func TestSyntheticDispatchRejectsSuppliedManagedPROutputKeys(t *testing.T) {
	store := minimalDispatchStore()
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "verify",
		Reason:       "bad synthetic verify",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{{
			Phase:        "prepare",
			PhaseOutputs: map[string]string{"pr_number": "338"},
		}},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot supply managed PR output") || !strings.Contains(rec.Body.String(), "pr_number") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}

func TestSyntheticDispatchRejectsMissingSlotLease(t *testing.T) {
	store := minimalDispatchStore()
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "verify",
		Reason:       "break-glass retry verify",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "execution_context.slot_lease_ref required") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}

// TestSyntheticDispatchForwardsCallerGitRef is the core capability: a synthetic
// dispatch that supplies inputs={git_ref: X} resolves the run's git_ref input to
// X and renders every `${{ inputs.git_ref }}` phase checkout ref to X at launch
// time. That is the mechanism for testing a harness branch via a cheap synthetic
// run without paying for the LLM phases. The assertion runs the exact production
// rendering the launcher runs (derivePrimaryCheckoutRepo + resolveRunnerCheckout-
// RunInputs over launchRunInputs) so it proves the wire-up end to end.
func TestSyntheticDispatchForwardsCallerGitRef(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.DispatchInputs = []DispatchInputSpec{{Name: "git_ref", Required: true, Default: "main"}}
	store.wf.Phases[0].Jobs[0].Checkout = &RunnerCheckoutSpec{Ref: CanonicalGitRefTemplate}
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "prepare",
		Reason:           "test harness branch via cheap synthetic run",
		Inputs:           map[string]string{"git_ref": "codex/harness-branch"},
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
	if got, want := store.runReq.RunInputs["git_ref"], "codex/harness-branch"; got != want {
		t.Fatalf("run input git_ref=%q, want %q", got, want)
	}
	if got, want := launcher.req.Run.RunInputs["git_ref"], "codex/harness-branch"; got != want {
		t.Fatalf("launch run input git_ref=%q, want %q", got, want)
	}
	// Render the launched start phase exactly as the Kubernetes launcher does and
	// confirm the `${{ inputs.git_ref }}` checkout ref resolves to the branch.
	rendered, err := derivePrimaryCheckoutRepo(launcher.req.Phase, launcher.req.Run.IssueRepo)
	if err != nil {
		t.Fatalf("derivePrimaryCheckoutRepo: %v", err)
	}
	rendered, err = resolveRunnerCheckoutRunInputs(rendered, launchRunInputs(launcher.req))
	if err != nil {
		t.Fatalf("resolveRunnerCheckoutRunInputs: %v", err)
	}
	if rendered.Jobs[0].Checkout == nil {
		t.Fatalf("rendered job has no checkout: %#v", rendered.Jobs[0])
	}
	if got, want := rendered.Jobs[0].Checkout.Ref, "codex/harness-branch"; got != want {
		t.Fatalf("rendered checkout.ref=%q, want %q", got, want)
	}
}

// TestSyntheticDispatchOmittedInputsDefaultToMain locks in that an existing
// synthetic dispatch (no inputs) is unchanged: declared defaults still apply, so
// git_ref resolves to main on the run and on the launcher payload.
func TestSyntheticDispatchOmittedInputsDefaultToMain(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.DispatchInputs = []DispatchInputSpec{{Name: "git_ref", Required: true, Default: "main"}}
	store.wf.Phases[0].Jobs[0].Checkout = &RunnerCheckoutSpec{Ref: CanonicalGitRefTemplate}
	launcher := &fakeRunLauncher{}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "prepare",
		Reason:           "existing synthetic dispatch with no inputs",
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
	if got, want := store.runReq.RunInputs["git_ref"], "main"; got != want {
		t.Fatalf("run input git_ref=%q, want default %q", got, want)
	}
	if got, want := launcher.req.Run.RunInputs["git_ref"], "main"; got != want {
		t.Fatalf("launch run input git_ref=%q, want default %q", got, want)
	}
	rendered, err := resolveRunnerCheckoutRunInputs(launcher.req.Phase, launchRunInputs(launcher.req))
	if err != nil {
		t.Fatalf("resolveRunnerCheckoutRunInputs: %v", err)
	}
	if got, want := rendered.Jobs[0].Checkout.Ref, "main"; got != want {
		t.Fatalf("rendered checkout.ref=%q, want default %q", got, want)
	}
}

// TestSyntheticDispatchRejectsUndeclaredInput mirrors the ordinary dispatch path:
// the synthetic boundary rejects an input the workflow's dispatch_inputs does not
// declare with the same 422 + "not declared" shape — no second, divergent
// validation path, no silent flow into Run.RunInputs.
func TestSyntheticDispatchRejectsUndeclaredInput(t *testing.T) {
	store := minimalDispatchStore()
	// minimalDispatchStore declares no dispatch_inputs.
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "prepare",
		Reason:           "undeclared input",
		Inputs:           map[string]string{"git_ref": "feature/branch"},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "git_ref") || !strings.Contains(rec.Body.String(), "not declared") {
		t.Fatalf("body=%s, want git_ref + not declared message", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("undeclared input should fail before creating run: %#v", store.runReq)
	}
}

// TestSyntheticDispatchRejectsInvalidRunInputs locks in that a structurally
// invalid input key is a 400 at the boundary, before any project/workflow read or
// run creation — symmetrical with the ordinary dispatch path.
func TestSyntheticDispatchRejectsInvalidRunInputs(t *testing.T) {
	store := minimalDispatchStore()
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "prepare",
		Reason:           "invalid input key",
		Inputs:           map[string]string{"bad key": "main"},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("invalid input should fail before creating run: %#v", store.runReq)
	}
}

// TestSyntheticDispatchRejectsMissingRequiredInput locks in the migration-policy
// "no fallback defaults" rule for the synthetic boundary: a required input with
// no declared default and no caller value is a 422, not a server-side guess.
func TestSyntheticDispatchRejectsMissingRequiredInput(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.DispatchInputs = []DispatchInputSpec{{Name: "git_ref", Required: true}}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:          "proj",
		IssueNumber:      7,
		WorkflowName:     "main",
		StartAtPhase:     "prepare",
		Reason:           "missing required input",
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "git_ref") || !strings.Contains(rec.Body.String(), "required") {
		t.Fatalf("body=%s, want git_ref + required message", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("missing required input should fail before creating run: %#v", store.runReq)
	}
}

func TestSyntheticDispatchRejectsUnsatisfiedStartInputs(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.Phases[1].Inputs = map[string]string{
		"issue_contract": "${{ phases.prepare.outputs.issue_contract }}",
	}
	body, _ := json.Marshal(SyntheticDispatchRequest{
		Project:      "proj",
		IssueNumber:  7,
		WorkflowName: "main",
		StartAtPhase: "verify",
		Reason:       "break-glass retry verify",
		SuppliedPhaseOutputs: []SyntheticSuppliedPhaseOutput{{
			Phase:        "prepare",
			PhaseOutputs: map[string]string{},
		}},
		ExecutionContext: SyntheticExecutionContext{SlotLeaseRef: "lease-1"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/synthetic-dispatch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")

	newSyntheticDispatchTestHandler(store, &fakeRunLauncher{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "start_at_phase inputs are not satisfied") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil {
		t.Fatalf("CreateRun should not be called: %#v", store.runReq)
	}
}
