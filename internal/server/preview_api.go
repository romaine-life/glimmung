package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/metrics"
)

// PreviewControlStore is the durable surface the preview control API needs.
// Satisfied by the runtime store.
type PreviewControlStore interface {
	CreatePreviewEnvironment(ctx context.Context, env PreviewEnvironment) (PreviewEnvironment, error)
	GetPreviewEnvironment(ctx context.Context, project, name string) (PreviewEnvironment, error)
	ListPreviewEnvironments(ctx context.Context) ([]PreviewEnvironment, error)
	UpdatePreviewEnvironmentIfMatch(ctx context.Context, project, name string, mutate func(PreviewEnvironment) (PreviewEnvironment, error)) (PreviewEnvironment, error)
	DeletePreviewEnvironment(ctx context.Context, project, name string) error
}

// The preview provision reads a single project row (live_preview metadata,
// repo, and the preview wildcard URL base) via the existing ProjectReader
// interface (project_scale_api.go).

type previewProvisionRequest struct {
	Project           string `json:"project"`
	Name              string `json:"name"`
	AuthorizedSubject string `json:"authorized_subject"`
	SessionID         string `json:"session_id"`
}

type previewPushReceiptRequest struct {
	Build string `json:"build"`
}

// provisionPreviewEnvironment is the session-initiated provision control: it
// records the durable preview_environment row (state=provisioning) and kicks
// the (control-plane-only) Helm provision in the background, returning the
// durable row immediately. "Live transport wakes; the durable row owns state":
// the caller resyncs from the row (or the SSE snapshot) rather than blocking on
// the multi-minute Helm install.
func provisionPreviewEnvironment(settings Settings, store ReadStore, provisioner PreviewProvisioner, minter RunnerGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controlStore, ok := store.(PreviewControlStore)
		if !ok || controlStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "preview store not configured")
			return
		}
		projectReader, ok := store.(ProjectReader)
		if !ok || projectReader == nil {
			writeProblem(w, http.StatusServiceUnavailable, "project reader not configured")
			return
		}
		// Provisioning deploys Helm into the shared runner namespace and
		// completes the durable row from a background goroutine — a
		// control-plane mutation. A slot process (ControlPlaneLoopsEnabled=false)
		// must never run it (Test Slots contract); it has no provisioner wired.
		if !settings.ControlPlaneLoopsEnabled || provisioner == nil {
			// Operational 503 (a slot process can't provision — provisioning is a
			// control-plane mutation), so surface it via writeUnavailable: it logs
			// + increments glimmung_unavailable_total{route,reason}, per the
			// deliberate-503 contract (scripts/check-503-observability.mjs).
			writeUnavailable(w, r, "preview provisioning runs on the glimmung control plane", "preview_control_plane_only")
			return
		}

		var req previewProvisionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(req.Project) == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "project is required")
			return
		}
		project, err := projectReader.ReadProject(r.Context(), strings.TrimSpace(req.Project))
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "unknown project: "+strings.TrimSpace(req.Project))
			return
		}
		cfg, enabled := livePreviewConfig(project)
		if !enabled {
			writeProblem(w, http.StatusUnprocessableEntity, "project does not enable the live_preview metadata key")
			return
		}
		name := sanitizePreviewName(firstNonEmpty(req.Name, req.SessionID, req.AuthorizedSubject))
		if name == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "a valid preview name (or session_id) is required")
			return
		}
		urlPtr := testSlotURL(project, &name)
		if urlPtr == nil {
			writeProblem(w, http.StatusUnprocessableEntity, "project has no preview wildcard base (metadata.runner_standby_dns.record_base)")
			return
		}
		subject := strings.TrimSpace(req.AuthorizedSubject)
		if subject == "" {
			if user, ok := adminUser(r.Context()); ok {
				subject = strings.TrimSpace(user.Sub)
			}
		}
		if subject == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "authorized_subject is required (the IdP subject permitted to push)")
			return
		}
		projectName := firstNonEmpty(project.Name, project.ID)
		env := PreviewEnvironment{
			Project:           projectName,
			Name:              name,
			LeaseRef:          "preview-" + projectName + "-" + name,
			SessionID:         strings.TrimSpace(req.SessionID),
			AuthorizedSubject: subject,
			Enabled:           true,
			State:             PreviewStateProvisioning,
			URL:               strings.TrimSpace(*urlPtr),
			// The edge upstream points at THIS app's own backend port (from its
			// live_preview.backend_port metadata), not a hardcoded :8000 — apps
			// listen on different ports (glimmung :8000, kill-me :3000, ambience
			// :8080). Unset falls back to defaultPreviewBackendPort.
			UpstreamURL:     previewUpstreamURL(cfg.BackendPort),
			BackendPrefixes: cfg.BackendPrefixes,
			EdgeImage:         strings.TrimSpace(settings.LivePreviewEdgeImageRepository) + ":" + strings.TrimSpace(settings.LivePreviewEdgeImageTag),
		}
		created, err := controlStore.CreatePreviewEnvironment(r.Context(), env)
		if err != nil {
			writeInternalError(w, r, err, "create preview environment failed")
			return
		}
		// Re-provision of an existing env: ensure it is back in provisioning and
		// re-enabled so the verifier resumes.
		if created.State != PreviewStateProvisioning {
			if reset, uerr := controlStore.UpdatePreviewEnvironmentIfMatch(r.Context(), created.Project, created.Name, func(cur PreviewEnvironment) (PreviewEnvironment, error) {
				cur.Enabled = true
				cur.AuthorizedSubject = subject
				return cur.MarkProvisioning(), nil
			}); uerr == nil {
				created = reset
			}
		}
		go runPreviewProvision(context.Background(), controlStore, provisioner, minter, created, project, log.Printf)
		writeJSON(w, http.StatusAccepted, created)
	}
}

// runPreviewProvision executes the Helm provision and folds the terminal result
// into durable state (ready or error), counting the provision outcome and
// waking the verifier so it begins reading the edge back. Exported at package
// scope (not a closure) so it is unit-testable synchronously.
func runPreviewProvision(ctx context.Context, store PreviewControlStore, provisioner PreviewProvisioner, minter RunnerGitHubTokenMinter, env PreviewEnvironment, project Project, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	provErr := provisioner.ProvisionPreview(ctx, env, project, minter)
	if _, err := store.UpdatePreviewEnvironmentIfMatch(ctx, env.Project, env.Name, func(cur PreviewEnvironment) (PreviewEnvironment, error) {
		if provErr != nil {
			return cur.MarkError(provErr.Error()), nil
		}
		// Provisioned clean: ready to serve the stable backend (fresh
		// passthrough) until the first push.
		return cur.MarkReady(), nil
	}); err != nil && logf != nil {
		logf("preview provision durable update failed project=%s name=%s err=%v", env.Project, env.Name, err)
	}
	if provErr != nil {
		metrics.RecordLivePreviewProvisioned(metrics.LivePreviewProvisionOutcomeError)
		if logf != nil {
			logf("preview provision failed project=%s name=%s err=%v", env.Project, env.Name, provErr)
		}
		return
	}
	metrics.RecordLivePreviewProvisioned(metrics.LivePreviewProvisionOutcomeOK)
	if logf != nil {
		logf("preview provisioned project=%s name=%s url=%s", env.Project, env.Name, env.URL)
	}
	wakePreviewVerify()
}

// recordPreviewPushReceipt records a session's claim that it pushed `build` to
// the edge. It does NOT mark the env live — it moves the env to `pushed` and
// wakes the verifier, which confirms (or finds stale) via the edge read-back.
func recordPreviewPushReceipt(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controlStore, ok := store.(PreviewControlStore)
		if !ok || controlStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "preview store not configured")
			return
		}
		project := strings.TrimSpace(r.PathValue("project"))
		name := strings.TrimSpace(r.PathValue("name"))
		var req previewPushReceiptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		build := strings.TrimSpace(req.Build)
		if build == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "build is required")
			return
		}
		updated, err := controlStore.UpdatePreviewEnvironmentIfMatch(r.Context(), project, name, func(cur PreviewEnvironment) (PreviewEnvironment, error) {
			return cur.RecordPushReceipt(build, time.Now()), nil
		})
		if writePreviewUpdateError(w, r, err) {
			return
		}
		metrics.RecordLivePreviewPushReceived()
		wakePreviewVerify()
		writeJSON(w, http.StatusOK, updated)
	}
}

// setPreviewEnabled toggles the preview lane on/off.
func setPreviewEnabled(store ReadStore, enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controlStore, ok := store.(PreviewControlStore)
		if !ok || controlStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "preview store not configured")
			return
		}
		project := strings.TrimSpace(r.PathValue("project"))
		name := strings.TrimSpace(r.PathValue("name"))
		updated, err := controlStore.UpdatePreviewEnvironmentIfMatch(r.Context(), project, name, func(cur PreviewEnvironment) (PreviewEnvironment, error) {
			return cur.SetEnabled(enabled), nil
		})
		if writePreviewUpdateError(w, r, err) {
			return
		}
		if enabled {
			wakePreviewVerify()
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

// getPreviewEnvironment reads the durable status of one preview env.
func getPreviewEnvironment(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controlStore, ok := store.(PreviewControlStore)
		if !ok || controlStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "preview store not configured")
			return
		}
		env, err := controlStore.GetPreviewEnvironment(r.Context(), strings.TrimSpace(r.PathValue("project")), strings.TrimSpace(r.PathValue("name")))
		if writePreviewUpdateError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, env)
	}
}

// listPreviewEnvironmentsHandler lists every preview env (the durable read).
func listPreviewEnvironmentsHandler(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controlStore, ok := store.(PreviewControlStore)
		if !ok || controlStore == nil {
			writeJSON(w, http.StatusOK, []PreviewEnvironment{})
			return
		}
		envs, err := controlStore.ListPreviewEnvironments(r.Context())
		if err != nil {
			writeInternalError(w, r, err, "list preview environments failed")
			return
		}
		if envs == nil {
			envs = []PreviewEnvironment{}
		}
		writeJSON(w, http.StatusOK, envs)
	}
}

// deletePreviewEnvironment deprovisions the preview env (control-plane runtime
// teardown) and removes the durable row.
func deletePreviewEnvironment(settings Settings, store ReadStore, provisioner PreviewProvisioner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		controlStore, ok := store.(PreviewControlStore)
		if !ok || controlStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "preview store not configured")
			return
		}
		projectReader, _ := store.(ProjectReader)
		project := strings.TrimSpace(r.PathValue("project"))
		name := strings.TrimSpace(r.PathValue("name"))
		env, err := controlStore.GetPreviewEnvironment(r.Context(), project, name)
		if writePreviewUpdateError(w, r, err) {
			return
		}
		// Best-effort runtime teardown when running on the control plane.
		if settings.ControlPlaneLoopsEnabled && provisioner != nil && projectReader != nil {
			if proj, perr := projectReader.ReadProject(r.Context(), env.Project); perr == nil {
				if derr := provisioner.DeprovisionPreview(r.Context(), env, proj); derr != nil {
					log.Printf("preview deprovision runtime teardown failed project=%s name=%s err=%v", env.Project, env.Name, derr)
				}
			}
		}
		if err := controlStore.DeletePreviewEnvironment(r.Context(), project, name); err != nil {
			writeInternalError(w, r, err, "delete preview environment failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "project": project, "name": name})
	}
}

// writePreviewUpdateError maps the store sentinels to HTTP problems. Returns
// true when an error response was written.
func writePreviewUpdateError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		writeProblem(w, http.StatusNotFound, "preview environment not found")
		return true
	case errors.Is(err, ErrPreconditionFailed):
		writeProblem(w, http.StatusConflict, "preview environment changed concurrently; retry")
		return true
	default:
		writeInternalError(w, r, err, "preview environment update failed")
		return true
	}
}

// sanitizePreviewName reduces a free-form name to a DNS-1035 label safe to use
// as a subdomain and Helm release/namespace name: lowercase, [a-z0-9-], must
// start with a letter, bounded length.
func sanitizePreviewName(in string) string {
	in = strings.ToLower(strings.TrimSpace(in))
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.' || r == '/' || r == ':':
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	// Must start with a letter (DNS-1035). Prefix when it doesn't.
	if name == "" {
		return ""
	}
	if name[0] < 'a' || name[0] > 'z' {
		name = "p-" + name
	}
	const maxLen = 40
	if len(name) > maxLen {
		name = strings.Trim(name[:maxLen], "-")
	}
	return name
}
