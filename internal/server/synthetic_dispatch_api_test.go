package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/auth"
)

func newSyntheticDispatchTestHandler(store ReadStore, nativeLauncher NativeLauncher) http.Handler {
	adminAuthenticator := fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/runs/synthetic-dispatch", requireAdmin(adminAuthenticator, http.HandlerFunc(syntheticDispatchRunHandler(Settings{}, store, nativeLauncher))))
	return mux
}

func TestSyntheticDispatchStartsAtRequestedPhaseWithSuppliedOutputs(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.Phases[1].Inputs = map[string]string{
		"issue_contract": "${{ phases.prepare.outputs.issue_contract }}",
	}
	launcher := &fakeNativeLauncher{}
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

	newSyntheticDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, req)

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

	newSyntheticDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, req)

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
