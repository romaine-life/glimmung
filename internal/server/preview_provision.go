package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// defaultPreviewBackendPort is the in-pod backend port the edge reverse-proxies
// to when an app's live_preview metadata does not set backend_port. It is
// glimmung's own backend port (k8s/issue deployment.yaml :8000) — the historical
// default — so an app that forgets to set backend_port still resolves to a
// concrete loopback port rather than silently breaking. Onboarding apps set
// their own (kill-me/chess-tactics :3000, ambience :8080).
const defaultPreviewBackendPort = 8000

// defaultPreviewUpstreamURL is the in-pod app backend base URL the edge
// reverse-proxies to when no per-app backend_port is set. The app backend
// listens internally (the edge is the pod's served port), so the upstream is
// loopback to the backend's container port.
const defaultPreviewUpstreamURL = "http://127.0.0.1:8000"

// previewUpstreamURL builds the edge's loopback upstream base URL for an app
// backend listening on backendPort. A non-positive port falls back to
// defaultPreviewBackendPort, so the result is always a concrete loopback URL the
// edge's LIVE_PREVIEW_EDGE_UPSTREAM / livePreview.upstream.url can use.
func previewUpstreamURL(backendPort int) string {
	if backendPort <= 0 {
		backendPort = defaultPreviewBackendPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d", backendPort)
}

// PreviewProvisioner deploys (and tears down) a preview environment: the stable
// app backend with the live-preview-edge in front of it, on the preview env's
// own wildcard URL, routing URL → edge → backend. It reuses the validation
// slot's Helm install machinery with a PREVIEW Helm config + a preview-typed
// (Kind=preview) in-memory lease, so the faithful image-deploy lane is
// untouched: a preview lease is never a runner/checkout lease, never reserves a
// runner slot, and is structurally not a validation target.
type PreviewProvisioner interface {
	// ProvisionPreview deploys the preview env (idempotent: a repeat call
	// re-reconciles the same Helm release).
	ProvisionPreview(ctx context.Context, env PreviewEnvironment, project Project, minter RunnerGitHubTokenMinter) error
	// DeprovisionPreview tears the preview env down.
	DeprovisionPreview(ctx context.Context, env PreviewEnvironment, project Project) error
}

// previewHelmValues builds the live-preview-edge `--set` values the consumer
// chart (k8s/issue + the live-preview-edge partial) reads to render the edge in
// front of the backend. The backend image is NOT set here — it comes from the
// chart's own pinned default (main's fingerprinted CI image, kept in lockstep
// with prod), so the preview backend is stable; only the pushed frontend is
// scratch.
func previewHelmValues(env PreviewEnvironment, edgeRepo, edgeTag string) map[string]string {
	values := map[string]string{
		"livePreview.enabled":           "true",
		"livePreview.image.repository":  strings.TrimSpace(edgeRepo),
		"livePreview.image.tag":         strings.TrimSpace(edgeTag),
		"livePreview.authorizedSubject": strings.TrimSpace(env.AuthorizedSubject),
		"livePreview.upstream.url":      firstNonEmpty(strings.TrimSpace(env.UpstreamURL), defaultPreviewUpstreamURL),
		// The preview backend is the STABLE main image run plainly — opt out of
		// the image-deploy lane's hot-swap supervisor for this lease (a chart
		// VALUE override; the hotSwapBackend template logic is untouched and the
		// validation lane keeps its default). Without this the preview pod would
		// also mount the hot-backend emptyDir and wrap the backend in the
		// supervisor, which the scratch-frontend preview lane does not use.
		"hotSwapBackend.enabled": "false",
	}
	for i, prefix := range env.BackendPrefixes {
		values[fmt.Sprintf("livePreview.backendPrefixes[%d]", i)] = prefix
	}
	return values
}

// previewHelmSettings layers the preview edge values onto the app's existing
// `test_slot_helm` install settings (chart path, installer image, RBAC). The
// preview reuses the app's knowledge of how to install its own chart and only
// adds the edge config — "borrow primitives, not boundaries". It is a pure
// function so the provision contract is unit-testable without a cluster.
func previewHelmSettings(project Project, env PreviewEnvironment, edgeRepo, edgeTag string) (testSlotHelmSettings, error) {
	base, ok := testSlotHelmConfig(project)
	if !ok {
		return testSlotHelmSettings{}, fmt.Errorf(
			"project %q has no test_slot_helm config; the preview lane reuses the app's chart install settings (chart_path, installer_image)",
			firstNonEmpty(project.Name, project.ID),
		)
	}
	if strings.TrimSpace(edgeRepo) == "" || strings.TrimSpace(edgeTag) == "" {
		return testSlotHelmSettings{}, fmt.Errorf("live-preview-edge image repository and tag are required to provision a preview")
	}
	merged := make(map[string]string, len(base.Values)+6)
	for k, v := range base.Values {
		merged[k] = v
	}
	for k, v := range previewHelmValues(env, edgeRepo, edgeTag) {
		merged[k] = v
	}
	base.Values = merged
	// The app chart includes the live-preview-edge partial; for an app whose chart
	// lives in ANOTHER repo (no in-repo file:// dependency), the install Job vendors
	// the partial from Glimmung's published ConfigMap. Glimmung's own slot chart
	// (k8s/issue) already vendors it via file://, so the install-time vendor step
	// no-ops there (it only runs when charts/live-preview-edge is absent).
	base.VendorLivePreviewEdge = true
	return base, nil
}

// previewLeaseFromEnv synthesizes the in-memory lease the Helm install
// machinery consumes (it reads runner_slot_name for substitutions and uses the
// lease ref for installer Job naming). It is durably typed Kind=preview and
// carries NONE of the runner/checkout markers (runner_k8s, test_slot_checkout),
// so it is excluded from every validation-target projection (the test-env
// snapshot, runner-slot availability, the checkout path). It is not persisted —
// the preview_environment row is the single durable source of truth; this lease
// is the ephemeral install-time handle.
func previewLeaseFromEnv(env PreviewEnvironment) Lease {
	return Lease{
		Kind:    LeaseKindPreview,
		Project: env.Project,
		State:   "active",
		Host:    strPtrOrNil(env.URL),
		Metadata: map[string]any{
			"runner_slot_name":   env.Name,
			"live_preview":       true,
			"authorized_subject": env.AuthorizedSubject,
			"tank_session_id":    env.SessionID,
			"preview_lease_ref":  env.LeaseRef,
		},
	}
}

func strPtrOrNil(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// ProvisionPreview deploys the preview env via the shared Helm install path with
// the preview config + the preview-typed lease, reconciling the SAME warm→hot
// sequence the faithful validation activation uses (run_launcher.go
// ActivateTestSlotRuntime, ~warm then ~hot). The two phases render disjoint,
// complementary slices of the slot chart, gated by renderMode in every app slot
// chart (including glimmung's own k8s/issue):
//
//   - WARM renders the route/cert/listener for the preview's wildcard host —
//     the HTTPRoute + testenv prerequisites are renderWarm-gated, so this is the
//     ONLY phase that materializes the route. Its hostname is the preview env's
//     own URL (testSlotSubstitutions derives {host} from the preview lease's
//     slot name, which is env.Name → env.URL).
//   - HOT renders the workload (Deployment + Service) with the vendored
//     live-preview-edge in front of the stable backend. The edge partial
//     self-gates on livePreview.enabled, which the preview values turn on, so
//     the edge becomes the served port and its override emptyDir lands in the
//     pod.
//
// A single hot-only install (the prior behavior) materialized the Deployment +
// Service but NO HTTPRoute, so the preview URL was unreachable. The warm phase
// is what makes the URL routable. The faithful validation reconcile path is
// untouched; this mirrors its sequencing for the preview-typed (Kind=preview)
// lease, which remains structurally not a validation target.
func (l *KubernetesRunLauncher) ProvisionPreview(ctx context.Context, env PreviewEnvironment, project Project, minter RunnerGitHubTokenMinter) error {
	name := strings.TrimSpace(env.Name)
	if name == "" {
		return fmt.Errorf("preview env name is required to provision")
	}
	if strings.TrimSpace(project.GitHubRepo) == "" {
		return fmt.Errorf("github_repo is required for preview provision")
	}
	if minter == nil {
		return fmt.Errorf("github token minter is required for preview provision")
	}
	config, err := previewHelmSettings(project, env, l.Settings.LivePreviewEdgeImageRepository, l.Settings.LivePreviewEdgeImageTag)
	if err != nil {
		return err
	}
	lease := previewLeaseFromEnv(env)
	// Namespaces + installer RBAC, reusing the validation slot's preliminary
	// access (for glimmung this is namespace creation + installer SA access;
	// it carries no validation semantics).
	if err := l.ensureTestSlotPreliminaryAccess(ctx, lease, project, name); err != nil {
		return err
	}
	// A preview backend that authenticates to Azure via workload identity (Postgres,
	// App Config, ...) needs a federated identity credential for THIS preview
	// namespace's service account before it boots — the standby pool's pre-created
	// credentials only cover the fixed standby slots, not ad-hoc preview namespaces.
	// Without it the backend gets AADSTS700213 / read-store-not-ready, never becomes
	// ready, and the HOT install times out. No-op for apps without
	// runner_standby_workload_identity. Cleaned up in DeprovisionPreview and on the
	// error paths below so a failed provision never leaks a credential.
	if l.WorkloadIdentity != nil {
		if err := l.WorkloadIdentity.EnsurePreviewWorkloadIdentity(ctx, project, name); err != nil {
			l.cleanupPreviewWorkloadIdentity(project, name)
			return fmt.Errorf("preview workload identity federation: %w", err)
		}
	}
	// WARM first: render the route/cert/listener for the preview's wildcard host
	// (renderWarm-gated in the slot chart). Without this phase a hot-only install
	// renders no HTTPRoute and the preview URL is unreachable.
	if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeWarm); err != nil {
		l.cleanupPreviewWorkloadIdentity(project, name)
		return err
	}
	// HOT second: render the workload + Service with the vendored edge in front
	// of the stable backend. Mirrors ActivateTestSlotRuntime's warm→hot.
	if err := l.runTestSlotHelmReconcile(ctx, lease, project, minter, config, testSlotRenderModeHot); err != nil {
		l.cleanupPreviewWorkloadIdentity(project, name)
		return err
	}
	return nil
}

// cleanupPreviewWorkloadIdentity best-effort deletes a preview's Azure federated
// identity credential on a DETACHED context (the provision context may already be
// cancelled or timed out), so a failed or torn-down preview does not leak a
// credential against the app identity. No-op when WI federation is not wired.
func (l *KubernetesRunLauncher) cleanupPreviewWorkloadIdentity(project Project, previewName string) {
	if l.WorkloadIdentity == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := l.WorkloadIdentity.RemovePreviewWorkloadIdentity(ctx, project, previewName); err != nil {
		log.Printf("preview workload identity cleanup failed project=%s name=%s err=%v",
			firstNonEmpty(project.Name, project.ID), previewName, err)
	}
}

// DeprovisionPreview tears down the preview env's runtime (installer Job, the
// preview namespace, and any slot-scoped access), reusing the validation slot
// teardown against the preview-typed lease.
func (l *KubernetesRunLauncher) DeprovisionPreview(ctx context.Context, env PreviewEnvironment, project Project) error {
	name := strings.TrimSpace(env.Name)
	if name == "" {
		return nil
	}
	// Remove the preview's federated identity credential alongside the runtime so
	// the app identity does not accumulate stale preview credentials (Azure caps
	// federated credentials per identity). Best-effort; logged, never blocks teardown.
	l.cleanupPreviewWorkloadIdentity(project, name)
	return l.DeprovisionTestSlot(ctx, previewLeaseFromEnv(env), project)
}
