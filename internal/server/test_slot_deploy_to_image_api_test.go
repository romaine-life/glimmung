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

func newDeployToImageStore(t *testing.T) *fakeLeaseStore {
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

// TestDeployTestSlotToImageHappyPath pins the dispatch contract: lease resolved
// by slot_name, ref resolved to a commit SHA, the deploy performer invoked with
// the resolved SHA (as both ref and image — CI tags by SHA) and the per-app
// image value key, and a 202 "running" returned with the resolved SHA.
func TestDeployTestSlotToImageHappyPath(t *testing.T) {
	store := newDeployToImageStore(t)
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
	handler := http.HandlerFunc(deployTestSlotToImage(store, nil, performer, resolveRef))
	body := `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedApplyRequest(t, body))

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
	if resp["status"] != "running" {
		t.Fatalf("resp.status = %v, want running", resp["status"])
	}
	// The poll handle the mcp tool drives the running→terminal transition with;
	// it must be present and tagged as a deploy job (shared by both history
	// entries so the apply-hot-swap status route serves the deploy unchanged).
	job, _ := resp["job"].(string)
	if !strings.HasPrefix(job, "deploy-") {
		t.Fatalf("resp.job = %v, want a deploy- handle", resp["job"])
	}
	select {
	case c := <-calls:
		if c.ref != "abc123def456" || c.image != "abc123def456" || c.key != "image.tag" {
			t.Fatalf("performer call = %+v, want {abc123def456 abc123def456 image.tag}", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("performer was not invoked")
	}
}

func TestDeployTestSlotToImageRequiresGitRef(t *testing.T) {
	store := newDeployToImageStore(t)
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployTestSlotToImage(store, nil, performer, resolveRef))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedApplyRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeployTestSlotToImageNoLease(t *testing.T) {
	store := newDeployToImageStore(t)
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployTestSlotToImage(store, nil, performer, resolveRef))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedApplyRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-9","git_ref":"feat/x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeployTestSlotToImageNotConfigured: a nil performer (launcher doesn't
// implement DeploySlotToImage) fails closed with 503, not a nil-call panic.
func TestDeployTestSlotToImageNotConfigured(t *testing.T) {
	store := newDeployToImageStore(t)
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployTestSlotToImage(store, nil, nil, resolveRef))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedApplyRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
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

// TestDeployTestSlotToImageRequiresHelmConfig: a project with a hot-swap
// contract but no test_slot_helm cannot be deployed (deploy reuses the helm
// reconcile, so it needs the chart config, not the retiring hot-swap contract).
func TestDeployTestSlotToImageRequiresHelmConfig(t *testing.T) {
	store := newApplyHotSwapStore(t) // has test_slot_hot_swap, not test_slot_helm
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployTestSlotToImage(store, nil, performer, resolveRef))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedApplyRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}
