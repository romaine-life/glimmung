package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShouldStartApplyHotSwapJobWatcherGate pins the Test Slots contract
// requirement: the finalizer is unreachable when ControlPlaneLoopsEnabled is
// false, so a slot process never finalizes prod apply-hot-swap Jobs. It also
// no-ops when the store cannot record hot-swap history.
func TestShouldStartApplyHotSwapJobWatcherGate(t *testing.T) {
	store := newApplyHotSwapStore(t) // implements StateStore + TestSlotHotSwapHistoryStore

	if shouldStartApplyHotSwapJobWatcher(Settings{ControlPlaneLoopsEnabled: false}, store) {
		t.Fatal("finalizer must be unreachable when ControlPlaneLoopsEnabled=false")
	}
	if !shouldStartApplyHotSwapJobWatcher(Settings{ControlPlaneLoopsEnabled: true}, store) {
		t.Fatal("finalizer should start when enabled and the store records hot-swap history")
	}
	// A read-only store with no lease/history capability is a no-op even when
	// the control plane is enabled.
	if shouldStartApplyHotSwapJobWatcher(Settings{ControlPlaneLoopsEnabled: true}, fakeReadStore{}) {
		t.Fatal("finalizer must no-op when the store cannot record hot-swap history")
	}
}

func terminalHotSwapJob(jobName, project, slot string, succeeded bool, reason string) map[string]any {
	cond := map[string]any{"type": "Complete", "status": "True"}
	if !succeeded {
		cond = map[string]any{"type": "Failed", "status": "True", "reason": reason}
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":      jobName,
			"namespace": "glimmung-runs",
			"labels": map[string]any{
				"app.kubernetes.io/name":                 hotSwapJobNameLabel,
				"glimmung.io/project":                    project,
				"glimmung.io/slot-name":                  slot,
				"glimmung.io/apply-hot-swap-kind":        "static",
				"glimmung.io/hot-swap-validation-target": "existing_session",
			},
		},
		"status": map[string]any{
			"conditions": []any{cond},
		},
	}
}

// TestDispatchHotSwapTerminalRecordsOutcome pins the finalizer's durable record:
// a completed apply-hot-swap Job joins back to its leased slot via the slot-name
// label, records the terminal outcome to history, and deletes the Job — all
// independent of any request.
func TestDispatchHotSwapTerminalRecordsOutcome(t *testing.T) {
	store := newApplyHotSwapStore(t)
	k8s := &fakeK8sJobClient{buildLogs: "build ok", swapLogs: "swap ok"}
	w := &k8sJobWatcher{watcherDeps: watcherDeps{
		namespace:      "glimmung-runs",
		logf:           func(string, ...any) {},
		hotSwapHistory: store,
		hotSwapState:   store,
		hotSwapK8s:     k8s,
	}}

	job := terminalHotSwapJob("apply-hot-swap-abc", "tank-operator", "tank-operator-slot-1", true, "")
	w.dispatchHotSwapTerminal(context.Background(), job, parseRunnerJobStatus(job), "list_sync")

	if got := store.leases[0].Metadata["last_hot_swap_status"]; got != "persisted" {
		t.Fatalf("finalizer recorded status = %v, want persisted", got)
	}
	if len(k8s.deleted) != 1 || k8s.deleted[0] != "apply-hot-swap-abc" {
		t.Fatalf("finalizer should delete the terminal job; deleted=%v", k8s.deleted)
	}
}

// TestDispatchHotSwapTerminalIsIdempotent pins the finalizer's guard against
// duplicate apiserver events / post-restart re-lists: when a terminal entry
// already exists for the job it does not append a second one, but still cleans
// up the Job.
func TestDispatchHotSwapTerminalIsIdempotent(t *testing.T) {
	store := newApplyHotSwapStore(t)
	// Pre-seed a terminal entry for the job and a sentinel last status.
	store.leases[0].Metadata["test_slot_hot_swap_history"] = []any{
		map[string]any{
			"operation":   "apply_hot_swap",
			"status":      "persisted",
			"diagnostics": map[string]any{"job_name": "apply-hot-swap-dup"},
		},
	}
	store.leases[0].Metadata["last_hot_swap_status"] = "sentinel"

	k8s := &fakeK8sJobClient{buildLogs: "build ok", swapLogs: "swap ok"}
	w := &k8sJobWatcher{watcherDeps: watcherDeps{
		namespace:      "glimmung-runs",
		logf:           func(string, ...any) {},
		hotSwapHistory: store,
		hotSwapState:   store,
		hotSwapK8s:     k8s,
	}}

	job := terminalHotSwapJob("apply-hot-swap-dup", "tank-operator", "tank-operator-slot-1", true, "")
	w.dispatchHotSwapTerminal(context.Background(), job, parseRunnerJobStatus(job), "modified")

	if got := store.leases[0].Metadata["last_hot_swap_status"]; got != "sentinel" {
		t.Fatalf("idempotent finalize must not append again; last status = %v, want sentinel", got)
	}
	if len(k8s.deleted) != 1 {
		t.Fatalf("idempotent finalize should still clean up the job; deleted=%v", k8s.deleted)
	}
}

// TestDispatchHotSwapTerminalTimeoutOutcome pins that a DeadlineExceeded Job
// failure (the activeDeadlineSeconds overrun) is recorded as a timeout.
func TestDispatchHotSwapTerminalTimeoutOutcome(t *testing.T) {
	store := newApplyHotSwapStore(t)
	k8s := &fakeK8sJobClient{buildLogs: "still building", swapLogs: ""}
	w := &k8sJobWatcher{watcherDeps: watcherDeps{
		namespace:      "glimmung-runs",
		logf:           func(string, ...any) {},
		hotSwapHistory: store,
		hotSwapState:   store,
		hotSwapK8s:     k8s,
	}}
	job := terminalHotSwapJob("apply-hot-swap-to", "tank-operator", "tank-operator-slot-1", false, "DeadlineExceeded")
	w.dispatchHotSwapTerminal(context.Background(), job, parseRunnerJobStatus(job), "modified")

	if got := store.leases[0].Metadata["last_hot_swap_status"]; got != "timeout" {
		t.Fatalf("finalizer recorded status = %v, want timeout", got)
	}
}

// TestGetApplyHotSwapStatusReturnsLatestEntry pins the poll surface: it returns
// the latest hot-swap history entry for a job (running → terminal) and 404s for
// an unknown job.
func TestGetApplyHotSwapStatusReturnsLatestEntry(t *testing.T) {
	store := newApplyHotSwapStore(t)
	store.leases[0].Metadata["test_slot_hot_swap_history"] = []any{
		map[string]any{
			"operation":   "apply_hot_swap",
			"status":      "running",
			"diagnostics": map[string]any{"job_name": "apply-hot-swap-poll"},
		},
		map[string]any{
			"operation":   "apply_hot_swap",
			"status":      "persisted",
			"diagnostics": map[string]any{"job_name": "apply-hot-swap-poll"},
		},
	}
	handler := getApplyHotSwapStatus(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/test-slots/apply-hot-swap/tank-operator/apply-hot-swap-poll", nil)
	req.SetPathValue("project", "tank-operator")
	req.SetPathValue("job", "apply-hot-swap-poll")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// The newest matching entry (persisted) is authoritative.
	if !strings.Contains(rec.Body.String(), `"status":"persisted"`) {
		t.Fatalf("status body should report persisted; got %s", rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/test-slots/apply-hot-swap/tank-operator/nope", nil)
	req2.SetPathValue("project", "tank-operator")
	req2.SetPathValue("job", "nope")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown job status = %d, want 404", rec2.Code)
	}
}
