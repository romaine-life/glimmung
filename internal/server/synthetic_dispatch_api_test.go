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
