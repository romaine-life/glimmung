package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func previewControlProject() Project {
	return Project{
		Name:       "glimmung",
		GitHubRepo: "romaine-life/glimmung",
		Metadata: map[string]any{
			"test_slot_helm": map[string]any{"enabled": true, "chart_path": "k8s/issue"},
			"live_preview":   map[string]any{"enabled": true, "backend_prefixes": []any{"/api"}},
			// Needed for testSlotURL to resolve the preview wildcard hostname.
			"runner_standby_dns": map[string]any{"record_base": "glimmung.dev.romaine.life"},
		},
	}
}

func controlPlaneSettings() Settings {
	return Settings{
		ControlPlaneLoopsEnabled:       true,
		LivePreviewEdgeImageRepository: "acr.io/edge",
		LivePreviewEdgeImageTag:        "edge",
	}
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSONPath(t, h, method, path, body, nil)
}

// doJSONPath invokes a handler with explicit path values (raw httptest requests
// do not run mux pattern matching, so PathValue must be set directly).
func doJSONPath(t *testing.T, h http.Handler, method, path string, body any, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProvisionPreviewValidations(t *testing.T) {
	store := newFakePreviewStore()
	store.putProject(previewControlProject())
	prov := &fakePreviewProvisioner{}
	h := provisionPreviewEnvironment(controlPlaneSettings(), store, prov, nil)

	// Missing project.
	if rec := doJSON(t, h, http.MethodPost, "/v1/previews", map[string]any{}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing project: code = %d", rec.Code)
	}
	// Unknown project.
	if rec := doJSON(t, h, http.MethodPost, "/v1/previews", map[string]any{"project": "nope", "authorized_subject": "s", "name": "x"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown project: code = %d", rec.Code)
	}
	// Missing authorized_subject (no admin user in ctx on a direct call).
	if rec := doJSON(t, h, http.MethodPost, "/v1/previews", map[string]any{"project": "glimmung", "name": "x"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing subject: code = %d", rec.Code)
	}
}

func TestProvisionPreviewGatedToControlPlane(t *testing.T) {
	store := newFakePreviewStore()
	store.putProject(previewControlProject())
	// Slot posture: control-plane loops disabled.
	h := provisionPreviewEnvironment(Settings{ControlPlaneLoopsEnabled: false}, store, &fakePreviewProvisioner{}, nil)
	rec := doJSON(t, h, http.MethodPost, "/v1/previews", map[string]any{"project": "glimmung", "name": "x", "authorized_subject": "s"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("slot posture must reject provisioning: code = %d", rec.Code)
	}
}

// TestPreviewContractFlow exercises the full provision -> push -> observe ->
// stale -> recover contract through the real control handlers + verifier.
func TestPreviewContractFlow(t *testing.T) {
	ctx := context.Background()
	store := newFakePreviewStore()
	store.putProject(previewControlProject())
	prov := &fakePreviewProvisioner{}

	// 1. Provision -> 202 Accepted, durable row in provisioning.
	provision := provisionPreviewEnvironment(controlPlaneSettings(), store, prov, nil)
	rec := doJSON(t, provision, http.MethodPost, "/v1/previews", map[string]any{
		"project": "glimmung", "name": "Session 42!", "authorized_subject": "svc:preview:owner",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("provision code = %d body=%s", rec.Code, rec.Body.String())
	}
	var created PreviewEnvironment
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	name := created.Name // sanitized to a DNS label
	if created.State != PreviewStateProvisioning {
		t.Fatalf("created state = %q, want provisioning", created.State)
	}
	if created.URL == "" || created.AuthorizedSubject != "svc:preview:owner" {
		t.Fatalf("created env missing url/subject: %+v", created)
	}

	// Wait for the background provision goroutine to flip the row to ready.
	waitForPreviewState(t, store, "glimmung", name, PreviewStateReady)

	// 2. Push receipt -> pushed (a claim, not yet live).
	receipt := recordPreviewPushReceipt(store)
	pv := map[string]string{"project": "glimmung", "name": name}
	rec = doJSONPath(t, receipt, http.MethodPost, "/v1/previews/glimmung/"+name+"/push-receipt", previewPushReceiptRequest{Build: "build-A"}, pv)
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt code = %d", rec.Code)
	}
	durable, _ := store.GetPreviewEnvironment(ctx, "glimmung", name)
	if durable.State != PreviewStatePushed || durable.LiveBuildID != "build-A" {
		t.Fatalf("after receipt: state=%q build=%q", durable.State, durable.LiveBuildID)
	}

	// 3. Observed read-back confirms live.
	reader := &stubStatusReader{byURL: map[string]PreviewEdgeStatus{durable.URL: {OverrideActive: true, Build: "build-A"}}}
	if _, err := VerifyPreviewEnvironment(ctx, store, reader, durable, time.Now, nil); err != nil {
		t.Fatalf("verify live: %v", err)
	}
	durable, _ = store.GetPreviewEnvironment(ctx, "glimmung", name)
	if durable.State != PreviewStateLive {
		t.Fatalf("after observe: state = %q, want live", durable.State)
	}

	// 4. Push build-B; edge still serving A -> stale.
	_ = doJSONPath(t, receipt, http.MethodPost, "/v1/previews/glimmung/"+name+"/push-receipt", previewPushReceiptRequest{Build: "build-B"}, pv)
	durable, _ = store.GetPreviewEnvironment(ctx, "glimmung", name)
	reader.byURL[durable.URL] = PreviewEdgeStatus{OverrideActive: true, Build: "build-A"}
	if _, err := VerifyPreviewEnvironment(ctx, store, reader, durable, time.Now, nil); err != nil {
		t.Fatalf("verify stale: %v", err)
	}
	durable, _ = store.GetPreviewEnvironment(ctx, "glimmung", name)
	if durable.State != PreviewStateStale {
		t.Fatalf("stale state = %q, want stale", durable.State)
	}

	// 5. Disable -> disabled; status read reflects it.
	disable := setPreviewEnabled(store, false)
	_ = doJSONPath(t, disable, http.MethodPost, "/v1/previews/glimmung/"+name+"/disable", nil, pv)
	get := getPreviewEnvironment(store)
	rec = doJSONPath(t, get, http.MethodGet, "/v1/previews/glimmung/"+name, nil, pv)
	var read PreviewEnvironment
	_ = json.Unmarshal(rec.Body.Bytes(), &read)
	if read.Enabled || read.State != PreviewStateDisabled {
		t.Fatalf("after disable: enabled=%v state=%q", read.Enabled, read.State)
	}
}

func TestRunPreviewProvisionRecordsTerminalState(t *testing.T) {
	ctx := context.Background()
	store := newFakePreviewStore()
	env := PreviewEnvironment{Project: "p", Name: "n", State: PreviewStateProvisioning, Enabled: true}
	_, _ = store.CreatePreviewEnvironment(ctx, env)

	// Success -> ready.
	runPreviewProvision(ctx, store, &fakePreviewProvisioner{}, nil, env, Project{Name: "p"}, nil)
	got, _ := store.GetPreviewEnvironment(ctx, "p", "n")
	if got.State != PreviewStateReady {
		t.Fatalf("success state = %q, want ready", got.State)
	}

	// Failure -> error with detail.
	runPreviewProvision(ctx, store, &fakePreviewProvisioner{err: errProvBoom}, nil, env, Project{Name: "p"}, nil)
	got, _ = store.GetPreviewEnvironment(ctx, "p", "n")
	if got.State != PreviewStateError || got.Detail == "" {
		t.Fatalf("failure state = %q detail = %q", got.State, got.Detail)
	}
}

var errProvBoom = &provBoomError{}

type provBoomError struct{}

func (*provBoomError) Error() string { return "provision boom" }

func TestSanitizePreviewName(t *testing.T) {
	cases := map[string]string{
		"Session 42!":     "session-42",
		"  spaces  ":      "spaces",
		"123-start":       "p-123-start",
		"a/b:c.d":         "a-b-c-d",
		"--leading--":     "leading",
		"UPPER":           "upper",
		"!!!":             "",
		"feature/foo_bar": "feature-foo-bar",
	}
	for in, want := range cases {
		if got := sanitizePreviewName(in); got != want {
			t.Fatalf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func waitForPreviewState(t *testing.T, store *fakePreviewStore, project, name, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		env, err := store.GetPreviewEnvironment(context.Background(), project, name)
		if err == nil && env.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("preview %s/%s did not reach state %q", project, name, want)
}
