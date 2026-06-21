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
								"workflow":   "docker-build-check.yaml",
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
	return func(context.Context, Project, string, string, string) (ResolvedTestSlotImage, error) {
		resolved, err := resolvedTestSlotImageFromRef(image, "test")
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		return resolved, nil
	}
}

func failingTestSlotImageResolver(err error) testSlotImageResolver {
	return func(context.Context, Project, string, string, string) (ResolvedTestSlotImage, error) {
		return ResolvedTestSlotImage{}, err
	}
}

// TestDeployImageToTestSlotChartSource pins the default-branch path: image_source
// "chart" deploys the chart at the ref with NO CI-image override (the chart's
// pinned production image stands), so it must NOT resolve a CI image — main has
// none (docker-build-check is PR-only). The performer is invoked with an empty
// override and empty image value key.
func TestDeployImageToTestSlotChartSource(t *testing.T) {
	store := newDeployImageStore(t)
	type performerCall struct{ ref, override, key string }
	calls := make(chan performerCall, 1)
	performer := func(_ context.Context, _ Lease, _ Project, verifiedRef, imageOverrideValue, imageValueKey string) error {
		calls <- performerCall{verifiedRef, imageOverrideValue, imageValueKey}
		return nil
	}
	resolveRef := func(_ context.Context, _, ref, _ string) (string, error) {
		if ref != "main" {
			return "", fmt.Errorf("unexpected ref=%s", ref)
		}
		return "mainsha", nil
	}
	// Must NOT be called for image_source=chart; fail loudly if it is.
	resolveImage := failingTestSlotImageResolver(fmt.Errorf("resolveImage must not be called for image_source=chart"))
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))
	body := `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"main","image_source":"chart"}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	select {
	case c := <-calls:
		if c.ref != "mainsha" {
			t.Fatalf("performer ref = %q, want mainsha (chart rendered at the ref)", c.ref)
		}
		if c.override != "" || c.key != "" {
			t.Fatalf("chart source must deploy with NO override; got override=%q key=%q", c.override, c.key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("performer was not invoked")
	}
}

// TestDeployImageToTestSlotHappyPath pins the dispatch contract: lease resolved
// by slot_name, ref resolved to a commit SHA, that SHA resolved to a validated
// CI lookup image, the deploy performer invoked with the resolved SHA as chart
// ref and a tag-only image override for image.tag, and a 202 "running" returned
// with both full-image and tag values.
func TestDeployImageToTestSlotHappyPath(t *testing.T) {
	store := newDeployImageStore(t)
	type performerCall struct{ ref, override, key string }
	calls := make(chan performerCall, 1)
	performer := func(_ context.Context, _ Lease, _ Project, verifiedRef, imageOverrideValue, imageValueKey string) error {
		calls <- performerCall{verifiedRef, imageOverrideValue, imageValueKey}
		return nil
	}
	resolveRef := func(_ context.Context, slug, ref, _ string) (string, error) {
		if slug != "romaine-life/tank-operator" || ref != "feat/x" {
			return "", fmt.Errorf("unexpected slug=%s ref=%s", slug, ref)
		}
		return "abc123def456", nil
	}
	resolveImage := stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:sha-abc123def456")
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))
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
	if resp["image"] != "romainecr.azurecr.io/tank-operator:sha-abc123def456" {
		t.Fatalf("resp.image = %v, want CI lookup image", resp["image"])
	}
	if resp["image_tag"] != "sha-abc123def456" {
		t.Fatalf("resp.image_tag = %v, want sha-abc123def456", resp["image_tag"])
	}
	if resp["image_override"] != "sha-abc123def456" {
		t.Fatalf("resp.image_override = %v, want sha-abc123def456", resp["image_override"])
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
		if c.ref != "abc123def456" || c.override != "sha-abc123def456" || c.key != "image.tag" {
			t.Fatalf("performer call = %+v, want {abc123def456 sha-abc123def456 image.tag}", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("performer was not invoked")
	}
}

func TestDeployImageToTestSlotExtendsShortLeaseToHotSwapMinimum(t *testing.T) {
	store := newDeployImageStore(t)
	assignedAt := time.Now().UTC().Add(-55 * time.Minute)
	store.leases[0].RequestedAt = assignedAt
	store.leases[0].AssignedAt = &assignedAt
	store.leases[0].TTLSeconds = 3600
	store.fakeReadStore.projects[0].Metadata[testLeaseProjectHotSwapMinTTLSecondsKey] = 3600
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "abc123def456", nil }
	resolveImage := stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:sha-abc123def456")
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if store.updatedRef != "tank-operator-slot-1" {
		t.Fatalf("updatedRef=%q, want tank-operator-slot-1", store.updatedRef)
	}
	expiresAt := assignedAt.Add(time.Duration(store.updatedTTL) * time.Second)
	if remaining := time.Until(expiresAt); remaining < 3590*time.Second {
		t.Fatalf("remaining TTL after deploy refresh=%s, want at least project hot-swap minimum", remaining)
	}
	if !strings.Contains(rec.Body.String(), `"lease_extended_by_seconds":`) {
		t.Fatalf("response=%s, want lease extension detail", rec.Body.String())
	}
}

func TestDeployImageToTestSlotDoesNotShortenSufficientLease(t *testing.T) {
	store := newDeployImageStore(t)
	assignedAt := time.Now().UTC().Add(-5 * time.Minute)
	store.leases[0].RequestedAt = assignedAt
	store.leases[0].AssignedAt = &assignedAt
	store.leases[0].TTLSeconds = 7200
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "abc123def456", nil }
	resolveImage := stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:sha-abc123def456")
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if store.updatedTTL != 0 {
		t.Fatalf("updatedTTL=%d, want no TTL write", store.updatedTTL)
	}
	if !strings.Contains(rec.Body.String(), `"lease_extended_by_seconds":0`) {
		t.Fatalf("response=%s, want zero lease extension detail", rec.Body.String())
	}
}

func TestDeployImageToTestSlotRejectsCleanupStartedLease(t *testing.T) {
	store := newDeployImageStore(t)
	now := time.Now().UTC()
	store.leases[0].RequestedAt = now
	store.leases[0].AssignedAt = &now
	store.leases[0].TTLSeconds = 3600
	ref := "tank-operator-slot-1"
	if _, err := store.CreateSlot(context.Background(), Slot{
		Project:        "tank-operator",
		SlotIndex:      1,
		SlotName:       "tank-operator-slot-1",
		State:          SlotStateCleaning,
		UpdatedAt:      now,
		ActiveLeaseRef: &ref,
	}); err != nil {
		t.Fatalf("seed slot: %v", err)
	}
	calls := make(chan struct{}, 1)
	performer := func(context.Context, Lease, Project, string, string, string) error {
		calls <- struct{}{}
		return nil
	}
	resolveRef := func(context.Context, string, string, string) (string, error) {
		calls <- struct{}{}
		return "abc123def456", nil
	}
	resolveImage := stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:sha-abc123def456")
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if store.updatedTTL != 0 {
		t.Fatalf("updatedTTL=%d, want no TTL write", store.updatedTTL)
	}
	select {
	case <-calls:
		t.Fatal("deploy should not resolve or perform after cleanup starts")
	default:
	}
}

func TestDeployImageToTestSlotUsesFullRefForNonTagImageValueKey(t *testing.T) {
	store := newDeployImageStore(t)
	project := &store.fakeReadStore.projects[0]
	project.Metadata["test_slot_deploy"].(map[string]any)["image_value_key"] = "edge.image"
	calls := make(chan string, 1)
	performer := func(_ context.Context, _ Lease, _ Project, _, imageOverrideValue, _ string) error {
		calls <- imageOverrideValue
		return nil
	}
	resolveRef := func(context.Context, string, string, string) (string, error) { return "abc123def456", nil }
	resolveImage := stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:sha-abc123def456")
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-calls:
		want := "romainecr.azurecr.io/tank-operator:sha-abc123def456"
		if got != want {
			t.Fatalf("override = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("performer was not invoked")
	}
}

func TestDeployImageToTestSlotRequiresGitRef(t *testing.T) {
	store := newDeployImageStore(t)
	performer := func(context.Context, Lease, Project, string, string, string) error { return nil }
	resolveRef := func(context.Context, string, string, string) (string, error) { return "sha", nil }
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
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
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
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
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, nil, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
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
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, stubTestSlotImageResolver("romainecr.azurecr.io/tank-operator:app-test")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeployImageToTestSlotReturns422WhenImageAbsent: a commit with no published
// CI image — here an in-progress build — is terminal at the deploy endpoint (422,
// not a special retryable status) and must not dispatch the deploy performer. The
// provisioning gate gates on the durable ci_image_available row, so glimmung no
// longer distinguishes a still-building "pending" image from an absent one.
func TestDeployImageToTestSlotReturns422WhenImageAbsent(t *testing.T) {
	store := newDeployImageStore(t)
	performer := func(context.Context, Lease, Project, string, string, string) error {
		t.Fatal("performer must not run when the CI image is absent")
		return nil
	}
	resolveRef := func(context.Context, string, string, string) (string, error) { return "racysha", nil }
	resolveImage := failingTestSlotImageResolver(fmt.Errorf("docker-build-check.yaml run 42 for commit racysha is in_progress; no sha-racysha image published yet"))
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image published yet") {
		t.Fatalf("body=%s, want the resolver's absent-image diagnostic", rec.Body.String())
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
	resolveImage := failingTestSlotImageResolver(fmt.Errorf("no CI image for commit unmappedsha in workflow docker-build-check.yaml: no build targets this commit (tag sha-unmappedsha absent)"))
	handler := http.HandlerFunc(deployImageToTestSlot(store, nil, nil, performer, resolveRef, resolveImage))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedDeployRequest(t, `{"project":"tank-operator","slot_name":"tank-operator-slot-1","git_ref":"feat/x"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no CI image for commit unmappedsha") {
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
