package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func authedDeployRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/test-slots/deploy-image", strings.NewReader(body))
}

func newDeployImageStore(t *testing.T) *fakeLeaseStore {
	t.Helper()
	return &fakeLeaseStore{
		fakeReadStore: fakeReadStore{
			projects: []Project{
				{
					Name:       "tank-operator",
					GitHubRepo: "romaine-life/tank-operator",
					Metadata: map[string]any{
						"test_slot_helm": map[string]any{
							"enabled":    true,
							"chart_path": "k8s",
						},
						"test_slot_deploy": map[string]any{
							"image_value_key": "image.tag",
							"ci_image": map[string]any{
								"repository": "romainecr.azurecr.io/tank-operator",
								"tags_by_sha": map[string]any{
									"abc123def456": "app-fingerprint123",
								},
							},
						},
					},
				},
			},
		},
		leases: []Lease{
			{
				ID:      "tank-operator-slot-1",
				Project: "tank-operator",
				State:   "claimed",
				Metadata: map[string]any{
					"test_slot_checkout": true,
					"runner_slot_name":   "tank-operator-slot-1",
					"runner_slot_index":  "1",
				},
			},
		},
	}
}

func stubTestSlotImageResolver(image string) testSlotImageResolver {
	return func(context.Context, Project, string) (ResolvedTestSlotImage, error) {
		resolved, err := resolvedTestSlotImageFromRef(image, "test")
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		return resolved, nil
	}
}

// TestDeployImageToTestSlotHappyPath pins the dispatch contract: lease resolved
// by slot_name, ref resolved to a commit SHA, that SHA resolved to a
// fingerprinted CI image from project metadata, the deploy performer invoked
// with the resolved SHA as chart ref and the fingerprint image as image, and a
// 202 "running" returned with both values.
func TestDeployImageToTestSlotHappyPath(t *testing.T) {
	store := newDeployImageStore(t)
	type performerCall struct{ ref, image, key string }
	calls := make(chan performerCall, 1)
	performer := func(_ context.Context, _ Lease, _ Project, verifiedRef, image, imageValueKey string) error {
		calls <- performerCall{verifiedRef, image, imageValueKey}
		return nil
	}
	resolveRef := func(_ context.Context, slug, ref, _ string) (string, error) {
		if slug != "romaine-life/tank-operator" || ref != "feat/x" {
			return "", fmt.Errorf("unexpected slug=%s ref=%s", slug, ref)
		}
		return "abc123def456", nil
	}
	resolveImage := projectMetadataTestSlotImageResolver(noopTestSlotImageValidator{})
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, performer, resolveRef, resolveImage))
	body := `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp["sha"] != "abc123def456" {
		t.Fatalf("resp.sha = %v, want abc123def456", resp["sha"])
	}
	if resp["image"] != "romainecr.azurecr.io/tank-operator:app-fingerprint123" {
		t.Fatalf("resp.image = %v, want fingerprint image", resp["image"])
	}
	if resp["image_tag"] != "app-fingerprint123" {
		t.Fatalf("resp.image_tag = %v, want app-fingerprint123", resp["image_tag"])
	}
	if resp["status"] != "running" {
		t.Fatalf("resp.status = %v, want running", resp["status"])
	}
	// The poll handle the mcp tool drives the running→terminal transition with;
	// it must be present and tagged as a deploy job (shared by both history
	// entries so the job-status route serves the deploy unchanged).
	job, _ := resp["job"].(string)
	if !strings.HasPrefix(job, "deploy-") {
		t.Fatalf("resp.job = %v, want a deploy- handle", resp["job"])
	}
	select {
	case c := <-calls:
		if c.ref != "abc123def456" || c.image != "romainecr.azurecr.io/tank-operator:app-fingerprint123" || c.key != "image.tag" {
			t.Fatalf("performer call = %+v, want {abc123def456 romainecr.azurecr.io/tank-operator:app-fingerprint123 image.tag}", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("performer was not invoked")
	}
}

func TestDeployImageToTestSlotRequiresGitRef(t *testing.T) {
	store := newDeployImageStore(t)
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, performer, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeployImageToTestSlotNoLease(t *testing.T) {
	store := newDeployImageStore(t)
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, performer, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-9","git_ref":"feat/x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeployImageToTestSlotNotConfigured: a nil performer (launcher doesn't
// implement DeployImageToSlot) fails closed with 503, not a nil-call panic.
func TestDeployImageToTestSlotNotConfigured(t *testing.T) {
	store := newDeployImageStore(t)
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestTestSlotDeployImageValueKeyDefaultsToImageTag: the deploy image-value key
// defaults to "image.tag" — the universal chart convention the chart-image-tag
// drift fix (glimmung#622) standardized on — so a standard project needs no
// per-app test_slot_deploy config, while a non-standard chart overrides it.
func TestTestSlotDeployImageValueKeyDefaultsToImageTag(t *testing.T) {
	if got := testSlotDeployImageValueKey(Project{Name: "p"}); got != "image.tag" {
		t.Fatalf("default key = %q, want image.tag", got)
	}
	override := Project{Name: "p", Metadata: map[string]any{
		"test_slot_deploy": map[string]any{"image_value_key": "edge.image"},
	}}
	if got := testSlotDeployImageValueKey(override); got != "edge.image" {
		t.Fatalf("override key = %q, want edge.image", got)
	}
}

// TestDeployImageToTestSlotRequiresHelmConfig: a project without test_slot_helm
// cannot be deployed because deploy reuses the helm reconcile.
func TestDeployImageToTestSlotRequiresHelmConfig(t *testing.T) {
	store := newDeployImageStore(t)
	store.projects[0].Metadata = map[string]any{}
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, performer, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeployImageToTestSlotRequiresResolvedValidatedFingerprintImage(t *testing.T) {
	store := newDeployImageStore(t)
	calls := make(chan struct{}, 1)
	performer := func(context.Context, Lease, Project, string, string, string) error {
		calls <- struct{}{}
		return nil
	}
	resolveRef := func(context.Context, string, string, string) (string, error) { return "unmappedsha", nil }
	resolveImage := projectMetadataTestSlotImageResolver(noopTestSlotImageValidator{})
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, performer, resolveRef, resolveImage))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no CI image mapping") {
		t.Fatalf("body=%s, want resolver failure", rec.Body.String())
	}
	select {
	case <-calls:
		t.Fatal("performer should not be invoked when image resolution fails")
	default:
	}
	if got := mapStringValueOrEmpty(store.leases[0].Metadata, "last_slot_op_status"); got != "" {
		t.Fatalf("history mutated with status %q before image resolution succeeded", got)
	}
}
