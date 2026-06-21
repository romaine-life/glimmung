package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func seededVerifierStore(t *testing.T, env PreviewEnvironment) *fakePreviewStore {
	t.Helper()
	store := newFakePreviewStore()
	if _, err := store.CreatePreviewEnvironment(context.Background(), env); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return store
}

func TestVerifyConfirmsLive(t *testing.T) {
	env := PreviewEnvironment{Project: "p", Name: "n", Enabled: true, URL: "https://n.example/", State: PreviewStatePushed, LiveBuildID: "build-A"}
	store := seededVerifierStore(t, env)
	reader := &stubStatusReader{byURL: map[string]PreviewEdgeStatus{
		"https://n.example/": {OverrideActive: true, Build: "build-A"},
	}}
	got, err := VerifyPreviewEnvironment(context.Background(), store, reader, env, time.Now, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.State != PreviewStateLive || got.ObservedBuildID != "build-A" {
		t.Fatalf("state=%q observed=%q, want live/build-A", got.State, got.ObservedBuildID)
	}
	// Durable row reflects the observed truth.
	durable, _ := store.GetPreviewEnvironment(context.Background(), "p", "n")
	if durable.State != PreviewStateLive {
		t.Fatalf("durable state = %q, want live", durable.State)
	}
}

func TestVerifyDetectsStale(t *testing.T) {
	env := PreviewEnvironment{Project: "p", Name: "n", Enabled: true, URL: "https://n.example/", State: PreviewStatePushed, LiveBuildID: "build-B"}
	store := seededVerifierStore(t, env)
	reader := &stubStatusReader{byURL: map[string]PreviewEdgeStatus{
		"https://n.example/": {OverrideActive: true, Build: "build-A"}, // edge serving the OLD build
	}}
	got, err := VerifyPreviewEnvironment(context.Background(), store, reader, env, time.Now, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.State != PreviewStateStale {
		t.Fatalf("state = %q, want stale", got.State)
	}
	if got.ObservedBuildID != "build-A" {
		t.Fatalf("observed = %q, want build-A (what the edge actually serves)", got.ObservedBuildID)
	}
}

func TestVerifyTransientReadErrorDoesNotFlipDurableState(t *testing.T) {
	env := PreviewEnvironment{Project: "p", Name: "n", Enabled: true, URL: "https://n.example/", State: PreviewStatePushed, LiveBuildID: "build-A"}
	store := seededVerifierStore(t, env)
	reader := &stubStatusReader{err: errors.New("connection refused")}
	if _, err := VerifyPreviewEnvironment(context.Background(), store, reader, env, time.Now, nil); err == nil {
		t.Fatalf("expected read error to propagate")
	}
	durable, _ := store.GetPreviewEnvironment(context.Background(), "p", "n")
	if durable.State != PreviewStatePushed {
		t.Fatalf("durable state = %q, want pushed (unchanged on transient read error)", durable.State)
	}
}

func TestDrainPreviewVerifyOnlyReadsPendingEnabledEnvs(t *testing.T) {
	store := newFakePreviewStore()
	ctx := context.Background()
	// pending + enabled -> read back
	_, _ = store.CreatePreviewEnvironment(ctx, PreviewEnvironment{Project: "p", Name: "pending", Enabled: true, URL: "https://pending/", State: PreviewStatePushed, LiveBuildID: "b"})
	// confirmed live (not pending) -> skip
	_, _ = store.CreatePreviewEnvironment(ctx, PreviewEnvironment{Project: "p", Name: "live", Enabled: true, URL: "https://live/", State: PreviewStateLive, LiveBuildID: "b", ObservedBuildID: "b"})
	// disabled -> skip
	_, _ = store.CreatePreviewEnvironment(ctx, PreviewEnvironment{Project: "p", Name: "off", Enabled: false, URL: "https://off/", State: PreviewStatePushed, LiveBuildID: "b"})
	reader := &stubStatusReader{byURL: map[string]PreviewEdgeStatus{"https://pending/": {OverrideActive: true, Build: "b"}}}
	drainPreviewVerify(ctx, store, reader, nil)
	if reader.calls() != 1 {
		t.Fatalf("read-back calls = %d, want 1 (only the pending+enabled env)", reader.calls())
	}
}

func TestPreviewVerifyReconcilerGate(t *testing.T) {
	// Slot posture: control-plane loops disabled -> never runs.
	if previewVerifyReconcilerShouldRun(Settings{ControlPlaneLoopsEnabled: false}, true, &stubStatusReader{}, nil) {
		t.Fatalf("reconciler must NOT run when ControlPlaneLoopsEnabled=false (Test Slots contract)")
	}
	// No reader -> disabled cleanly.
	if previewVerifyReconcilerShouldRun(Settings{ControlPlaneLoopsEnabled: true}, true, nil, nil) {
		t.Fatalf("reconciler must not run without a status reader")
	}
	// No preview-capable store -> disabled.
	if previewVerifyReconcilerShouldRun(Settings{ControlPlaneLoopsEnabled: true}, false, &stubStatusReader{}, nil) {
		t.Fatalf("reconciler must not run without a preview store")
	}
	// Control plane + store + reader -> runs.
	if !previewVerifyReconcilerShouldRun(Settings{ControlPlaneLoopsEnabled: true}, true, &stubStatusReader{}, nil) {
		t.Fatalf("reconciler should run on the control plane with store + reader")
	}
}

func TestHTTPPreviewStatusReaderReadsEdge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != previewStatusPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer svc-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(PreviewEdgeStatus{OverrideActive: true, Build: "build-Z", Release: "rel-1"})
	}))
	defer srv.Close()

	reader := NewHTTPPreviewStatusReader(staticTokenSource("svc-token"), srv.Client())
	status, err := reader.ReadStatus(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !status.OverrideActive || status.Build != "build-Z" {
		t.Fatalf("status = %+v", status)
	}
}

type staticTokenSource string

func (s staticTokenSource) Token(ctx context.Context) (string, error) { return string(s), nil }
