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

func TestPreviewUpstreamURLFromBackendPort(t *testing.T) {
	cases := map[int]string{
		8000: "http://127.0.0.1:8000",
		3000: "http://127.0.0.1:3000",
		8080: "http://127.0.0.1:8080",
		0:    defaultPreviewUpstreamURL, // unset → default backend port
		-1:   defaultPreviewUpstreamURL, // invalid → default backend port
	}
	for port, want := range cases {
		if got := previewUpstreamURL(port); got != want {
			t.Fatalf("previewUpstreamURL(%d) = %q, want %q", port, got, want)
		}
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

// TestPreviewInstallManifestVendorsEdgePartial proves a PREVIEW install Job
// carries the cross-repo vendoring: the ConfigMap mount + the vendor step that
// copies Glimmung's live-preview-edge partial into the app chart's charts/ when
// the chart does not already supply it (ACR Basic SKU blocks an oci:// pull).
func TestPreviewInstallManifestVendorsEdgePartial(t *testing.T) {
	project := previewTestProject()
	env := previewTestEnv()
	cfg, err := previewHelmSettings(project, env, "acr.io/edge", "edge-v1")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if !cfg.VendorLivePreviewEdge {
		t.Fatalf("previewHelmSettings must set VendorLivePreviewEdge for a preview install")
	}
	lease := previewLeaseFromEnv(env)
	settings := Settings{RunnerNamespace: "glimmung-runs", RunnerServiceAccount: "glimmung-runner"}
	subs := testSlotSubstitutions(lease, project, env.Name, env.Name+"-sessions")
	manifest := testSlotInstallJobManifest(settings, cfg, lease, project, subs, testSlotRenderModeHot)
	blob, _ := json.Marshal(manifest)
	script := string(blob)
	for _, want := range []string{
		// The vendor step is gated on the chart not already supplying the partial.
		"charts/live-preview-edge",
		// Sourced from the mounted ConfigMap Glimmung publishes.
		livePreviewEdgeChartConfigMapName,
		livePreviewEdgeChartMountPath,
		// Templates are rebuilt from tpl.* keys.
		"tpl.",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("preview install manifest missing vendor marker %q", want)
		}
	}
	// The ConfigMap volume is mounted optional so a missing partial fails in the
	// script with a clear message rather than wedging the pod on a mount error.
	if !strings.Contains(script, "\"optional\":true") {
		t.Fatalf("live-preview-edge ConfigMap volume must be optional: %s", script)
	}
}

// TestValidationInstallManifestHasNoEdgeVendoring pins that the faithful
// validation path is UNTOUCHED: a non-preview install (VendorLivePreviewEdge
// false) carries no vendor step and no ConfigMap mount.
func TestValidationInstallManifestHasNoEdgeVendoring(t *testing.T) {
	lease := Lease{Kind: "runner", Project: "glimmung", Metadata: map[string]any{"runner_slot_name": "glimmung-slot-1"}}
	project := Project{Name: "glimmung", GitHubRepo: "romaine-life/glimmung"}
	cfg := testSlotHelmSettings{ChartPath: "k8s/issue", InstallerImage: "alpine/k8s:1.30.0"}
	settings := Settings{RunnerNamespace: "glimmung-runs", RunnerServiceAccount: "glimmung-runner"}
	subs := testSlotSubstitutions(lease, project, "glimmung-slot-1", "glimmung-slot-1-sessions")
	manifest := testSlotInstallJobManifest(settings, cfg, lease, project, subs, testSlotRenderModeHot)
	blob, _ := json.Marshal(manifest)
	script := string(blob)
	for _, banned := range []string{livePreviewEdgeChartConfigMapName, livePreviewEdgeChartMountPath, "charts/live-preview-edge"} {
		if strings.Contains(script, banned) {
			t.Fatalf("validation install manifest must NOT carry edge vendoring, found %q", banned)
		}
	}
	// It still runs the normal dependency build (no-op for charts with no deps).
	if !strings.Contains(script, "helm dependency build") {
		t.Fatalf("validation install manifest missing helm dependency build")
	}
}
