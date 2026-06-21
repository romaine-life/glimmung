package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func previewTestProject() Project {
	return Project{
		Name:       "glimmung",
		GitHubRepo: "romaine-life/glimmung",
		Metadata: map[string]any{
			"test_slot_helm": map[string]any{
				"enabled":    true,
				"chart_path": "k8s/issue",
			},
			"live_preview": map[string]any{
				"enabled":          true,
				"backend_prefixes": []any{"/api", "/healthz"},
			},
		},
	}
}

func previewTestEnv() PreviewEnvironment {
	return PreviewEnvironment{
		Project:           "glimmung",
		Name:              "preview-glimmung-s1",
		AuthorizedSubject: "svc:preview:owner",
		BackendPrefixes:   []string{"/api", "/healthz"},
		UpstreamURL:       defaultPreviewUpstreamURL,
	}
}

func TestPreviewHelmValuesCarryEdgeConfig(t *testing.T) {
	values := previewHelmValues(previewTestEnv(), "acr.io/edge", "edge-v1")
	want := map[string]string{
		"livePreview.enabled":            "true",
		"livePreview.image.repository":   "acr.io/edge",
		"livePreview.image.tag":          "edge-v1",
		"livePreview.authorizedSubject":  "svc:preview:owner",
		"livePreview.upstream.url":       defaultPreviewUpstreamURL,
		"livePreview.backendPrefixes[0]": "/api",
		"livePreview.backendPrefixes[1]": "/healthz",
		// preview opts OUT of the image-deploy hot-swap supervisor (value only).
		"hotSwapBackend.enabled": "false",
	}
	for k, v := range want {
		if values[k] != v {
			t.Fatalf("value %q = %q, want %q", k, values[k], v)
		}
	}
}

func TestPreviewHelmSettingsLayerOntoBaseConfig(t *testing.T) {
	cfg, err := previewHelmSettings(previewTestProject(), previewTestEnv(), "acr.io/edge", "edge-v1")
	if err != nil {
		t.Fatalf("previewHelmSettings: %v", err)
	}
	// Reuses the app's chart install knowledge.
	if cfg.ChartPath != "k8s/issue" {
		t.Fatalf("chart path = %q, want k8s/issue (from the app's test_slot_helm)", cfg.ChartPath)
	}
	// And layers the edge values on top.
	if cfg.Values["livePreview.enabled"] != "true" {
		t.Fatalf("layered values missing livePreview.enabled: %v", cfg.Values)
	}
	if cfg.Values["livePreview.backendPrefixes[1]"] != "/healthz" {
		t.Fatalf("layered prefixes missing: %v", cfg.Values)
	}
}

func TestPreviewHelmSettingsRequiresTestSlotHelm(t *testing.T) {
	project := Project{Name: "noslotchart", Metadata: map[string]any{
		"live_preview": map[string]any{"enabled": true},
	}}
	if _, err := previewHelmSettings(project, previewTestEnv(), "r", "t"); err == nil {
		t.Fatalf("expected error when project has no test_slot_helm config")
	}
}

func TestPreviewHelmSettingsRequiresEdgeImage(t *testing.T) {
	if _, err := previewHelmSettings(previewTestProject(), previewTestEnv(), "", ""); err == nil {
		t.Fatalf("expected error when edge image repo/tag empty")
	}
}

// TestPreviewLeaseIsNotAValidationTarget pins the structural separation: a
// preview lease is durably typed `preview` and carries NONE of the runner /
// checkout markers, so every validation-target projection excludes it.
func TestPreviewLeaseIsNotAValidationTarget(t *testing.T) {
	lease := previewLeaseFromEnv(previewTestEnv())
	if lease.Kind != LeaseKindPreview {
		t.Fatalf("lease kind = %q, want %q", lease.Kind, LeaseKindPreview)
	}
	if _, ok := lease.Metadata["runner_k8s"]; ok {
		t.Fatalf("preview lease must NOT carry runner_k8s (would make it a runner-slot validation target)")
	}
	if _, ok := lease.Metadata["test_slot_checkout"]; ok {
		t.Fatalf("preview lease must NOT carry test_slot_checkout (would make it a checkout validation target)")
	}
	if boolFromMap(lease.Metadata, "runner_k8s") || boolFromMap(lease.Metadata, "test_slot_checkout") {
		t.Fatalf("preview lease must be excluded from testEnvironmentsFromSnapshot's claimed-lease filter")
	}
	// It still carries the slot name the Helm machinery substitutes.
	if v, _ := stringFromMap(lease.Metadata, "runner_slot_name"); v != "preview-glimmung-s1" {
		t.Fatalf("runner_slot_name = %q, want the preview env name", v)
	}
}

// TestPreviewInstallManifestCarriesEdgeSetFlags proves the shared install-job
// machinery, fed the preview config + preview lease, emits a helm command with
// the live-preview-edge --set flags and the renderMode=hot the chart needs.
func TestPreviewInstallManifestCarriesEdgeSetFlags(t *testing.T) {
	project := previewTestProject()
	env := previewTestEnv()
	cfg, err := previewHelmSettings(project, env, "acr.io/edge", "edge-v1")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	lease := previewLeaseFromEnv(env)
	settings := Settings{RunnerNamespace: "glimmung-runs", RunnerServiceAccount: "glimmung-runner"}
	subs := testSlotSubstitutions(lease, project, env.Name, env.Name+"-sessions")
	manifest := testSlotInstallJobManifest(settings, cfg, lease, project, subs, testSlotRenderModeHot)
	blob, _ := json.Marshal(manifest)
	script := string(blob)
	for _, want := range []string{
		"livePreview.enabled=true",
		"livePreview.image.repository=acr.io/edge",
		"livePreview.authorizedSubject=svc:preview:owner",
		"livePreview.backendPrefixes[0]=/api",
		"hotSwapBackend.enabled=false",
		"renderMode=hot",
		"helm dependency build",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install manifest missing %q", want)
		}
	}
}
