package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DeployImageToTestSlotRequest is the deploy-image-to-slot request body. The
// operation deploys the whole CI-built image for a verified commit, so there is
// no per-artifact selector.
type DeployImageToTestSlotRequest struct {
	Project   string  `json:"project"`
	SlotIndex *int    `json:"slot_index,omitempty"`
	SlotName  *string `json:"slot_name,omitempty"`
	GitRef    string  `json:"git_ref"`
	// ImageSource selects what image the slot runs. "" / "ci" (default) resolves
	// the CI-built proof image for the commit — the PR-deploy path. "chart"
	// deploys the chart at git_ref with NO image override: the chart's own pinned
	// image stands. Use "chart" for default-branch/main deploys, where no
	// per-commit CI proof image exists (docker-build-check is PR-only) but the
	// chart already pins the production image — so the slot runs exactly what
	// prod ships.
	ImageSource string `json:"image_source,omitempty"`
}

// deployImagePerformer is the function seam the test harness stubs. Production
// wires it to KubernetesRunLauncher.DeployImageToSlot. It reconciles the slot's
// chart at the verified ref with the CI image override pinned, then verifies the
// running image — a Job-backed operation that can run minutes, so the handler
// runs it detached and records the outcome durably rather than holding the
// request open.
type deployImagePerformer func(ctx context.Context, lease Lease, project Project, verifiedRef, imageOverrideValue, imageValueKey string) error

// refResolver resolves a git ref to its commit SHA. Production wires a live
// GitHub call (githubResolveSHA); tests stub it.
type refResolver func(ctx context.Context, slug, ref, token string) (string, error)

// testSlotImageResolver resolves the verified commit SHA to the CI-built image
// ref that the slot should run. Production uses GitHub Actions run metadata
// backed by a registry validation check; tests stub it so the HTTP contract
// stays narrow.
type testSlotImageResolver func(ctx context.Context, project Project, slug, sha, token string) (ResolvedTestSlotImage, error)

// imageToSlotDeployer is the concrete-launcher capability the deploy route wires
// its performer from. *KubernetesRunLauncher implements it; the route type-
// asserts rather than widening TestSlotPreparer so the test fakes are untouched.
type imageToSlotDeployer interface {
	DeployImageToSlot(ctx context.Context, lease Lease, project Project, minter RunnerGitHubTokenMinter, verifiedRef, image, imageValueKey string) error
}

// deployImageToTestSlot is the deploy-image-to-slot endpoint. It deploys the
// exact CI-built image for a verified commit onto a slot and verifies the slot
// runs it. Async-with-poll: the POST resolves the ref to a SHA, dispatches the
// deploy on a detached context, writes an initial "running" history entry, and
// returns 202 with that breadcrumb; the detached worker writes the terminal
// entry when the reconcile-and-verify completes. The caller polls
// GET /v1/test-slots/jobs/{project}/{job} via the lease history, so no HTTP
// request is held open for the deploy and the durable outcome survives client
// disconnects and proxy deadlines.
//
// Legitimacy — published + CI-green + mergeable + current-with-main — is
// enforced upstream by Tank's deterministic readiness gate, not here: the sole
// caller is Tank's server-side test-slot provisioning gate
// (provisionTestSlotForSession), which validates readiness through its CI-watch
// readiness reducer before it ever calls this endpoint. This endpoint stays a
// pure provisioner: it only ever operates on a git ref (never an agent working
// tree), so it cannot deploy unpushed code, and the SHA→image resolution deploys
// exactly the image CI built for that commit. It deliberately does not re-derive
// or own the legitimacy gate.
func deployImageToTestSlot(store ReadStore, preparer TestSlotPreparer, minter RunnerGitHubTokenMinter, performer deployImagePerformer, resolveRef refResolver, resolveImage testSlotImageResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(TestSlotOpHistoryStore)
		stateStore, hasState := store.(StateStore)
		updater, hasTTLUpdater := store.(LeaseTTLUpdater)
		if !ok || writer == nil || !hasState || stateStore == nil || !hasTTLUpdater || updater == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot history or TTL store not configured")
			return
		}
		if performer == nil || resolveRef == nil || resolveImage == nil {
			writeProblem(w, http.StatusServiceUnavailable, "deploy-image-to-slot not configured (run launcher has no slot deployer)")
			return
		}
		var req DeployImageToTestSlotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		req.GitRef = strings.TrimSpace(req.GitRef)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if req.GitRef == "" {
			writeProblem(w, http.StatusBadRequest, "git_ref required")
			return
		}

		lease, err := resolveTestSlotLease(r, stateStore, TestSlotReturnRequest{
			Project:   req.Project,
			SlotIndex: req.SlotIndex,
			SlotName:  req.SlotName,
		})
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "test slot lease not found")
				return
			}
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		leaseRef := LeasePublicRefFromLease(lease)
		slotName := strings.TrimSpace(mapStringValueOrEmpty(lease.Metadata, "runner_slot_name"))
		if slotName == "" {
			writeProblem(w, http.StatusBadRequest, "lease has no runner_slot_name (cannot derive target namespace)")
			return
		}

		projects, err := store.ListProjects(r.Context())
		if err != nil {
			writeInternalError(w, r, err, "list projects: "+err.Error())
			return
		}
		var project Project
		for _, p := range projects {
			if p.Name == req.Project {
				project = p
				break
			}
		}
		if project.Name == "" {
			writeProblem(w, http.StatusNotFound, "project not found")
			return
		}
		if _, ok := testSlotHelmConfig(project); !ok {
			writeProblem(w, http.StatusUnprocessableEntity, "project has no enabled test_slot_helm config")
			return
		}
		slug := strings.TrimSpace(project.GitHubRepo)
		if slug == "" {
			writeProblem(w, http.StatusUnprocessableEntity, "project has no github_repo")
			return
		}
		imageValueKey := testSlotDeployImageValueKey(project)
		leaseExtendedBy, err := ensureTestSlotHotSwapLeaseTTL(r.Context(), store, preparer, minter, project, lease)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "test slot lease not found")
			return
		case errors.Is(err, ErrConflict):
			writeProblem(w, http.StatusConflict, "test slot lease cannot be refreshed")
			return
		case err != nil:
			writeInternalError(w, r, err, "refresh test-slot lease before deploy failed")
			return
		}
		if leaseExtendedBy > 0 {
			if current, ok, err := currentTestSlotLease(r.Context(), store, lease); err == nil && ok {
				lease = current
			}
		}

		// Resolve the ref to its commit SHA, then resolve that SHA to the
		// fingerprinted image CI built. Raw SHA tags are not a deploy contract:
		// the image resolver must return a concrete, validated CI image before
		// this endpoint records a running operation or mutates the slot.
		repoToken := ""
		if minter != nil {
			tok, err := minter.RepositoryInstallationToken(r.Context(), slug, map[string]string{"contents": "read", "actions": "read"})
			if err != nil {
				writeInternalError(w, r, err, "mint clone token for deploy: "+err.Error())
				return
			}
			repoToken = tok
		}
		sha, err := resolveRef(r.Context(), slug, req.GitRef, repoToken)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "resolve git_ref to commit sha: "+err.Error())
			return
		}
		// Select the image to run. "chart" deploys the chart at the ref with NO
		// override — its pinned image stands (the default-branch path, where no
		// per-commit CI proof image exists but the chart already pins production).
		// Otherwise resolve the CI-built image for the commit (the PR-deploy path).
		var resolvedImage ResolvedTestSlotImage
		image := ""
		if strings.EqualFold(strings.TrimSpace(req.ImageSource), "chart") {
			imageValueKey = ""
			resolvedImage = ResolvedTestSlotImage{Source: "chart_default"}
		} else {
			resolvedImage, err = resolveImage(r.Context(), project, slug, sha, repoToken)
			if err != nil {
				writeProblem(w, http.StatusUnprocessableEntity, "resolve commit sha to CI image: "+err.Error())
				return
			}
			image = resolvedImage.Image
		}
		imageOverrideValue := testSlotDeployImageOverrideValue(resolvedImage, imageValueKey)
		// Pollable handle: both history entries carry it as the job_name, so the
		// slot job status route serves the deploy's running to terminal transition.
		deployJob := "deploy-" + uuid.NewString()

		// Dispatch detached: a client disconnect must not abort the deploy or the
		// durable history write. The reconcile itself is a Kubernetes Job, so the
		// work survives this process too; only the terminal-outcome write rides
		// the goroutine (a process restart mid-deploy leaves the "running"
		// breadcrumb, and a re-deploy is idempotent — helm upgrade --install).
		bgCtx := context.WithoutCancel(r.Context())
		startEntry := TestSlotOpHistoryEntry{
			Operation: "image_deploy",
			Status:    "running",
			Summary:   fmt.Sprintf("image_deploy dispatched git_ref=%s sha=%s slot=%s status=running", req.GitRef, sha, slotName),
			Diagnostics: map[string]any{
				"job_name":                  deployJob,
				"slot_name":                 slotName,
				"git_ref":                   req.GitRef,
				"sha":                       sha,
				"image":                     image,
				"image_tag":                 resolvedImage.Tag,
				"image_override":            imageOverrideValue,
				"image_source":              resolvedImage.Source,
				"image_value_key":           imageValueKey,
				"lease_ttl_seconds":         lease.TTLSeconds,
				"lease_extended_by_seconds": leaseExtendedBy,
			},
			CreatedAt: time.Now().UTC(),
		}
		if leaseWithHistory, histErr := writer.AppendTestSlotOpHistory(bgCtx, req.Project, leaseRef, startEntry); histErr == nil {
			lease = leaseWithHistory
		}

		deployLease := lease
		go func() {
			derr := performer(bgCtx, deployLease, project, sha, imageOverrideValue, imageValueKey)
			status := "deployed"
			diag := map[string]any{"job_name": deployJob, "slot_name": slotName, "git_ref": req.GitRef, "sha": sha, "image": image, "image_tag": resolvedImage.Tag, "image_override": imageOverrideValue, "image_source": resolvedImage.Source, "lease_ttl_seconds": deployLease.TTLSeconds, "lease_extended_by_seconds": leaseExtendedBy}
			if derr != nil {
				status = "deploy_failed"
				diag["error"] = derr.Error()
			}
			_, _ = writer.AppendTestSlotOpHistory(bgCtx, req.Project, leaseRef, TestSlotOpHistoryEntry{
				Operation:   "image_deploy",
				Status:      status,
				Summary:     fmt.Sprintf("image_deploy finalized git_ref=%s sha=%s slot=%s status=%s", req.GitRef, sha, slotName, status),
				Diagnostics: diag,
				CreatedAt:   time.Now().UTC(),
			})
		}()

		writeJSON(w, http.StatusAccepted, map[string]any{
			"lease":                     leaseRef,
			"job":                       deployJob,
			"status":                    "running",
			"git_ref":                   req.GitRef,
			"sha":                       sha,
			"image":                     image,
			"image_tag":                 resolvedImage.Tag,
			"image_override":            imageOverrideValue,
			"image_source":              resolvedImage.Source,
			"lease_ttl_seconds":         lease.TTLSeconds,
			"lease_extended_by_seconds": leaseExtendedBy,
			"history_entry":             startEntry,
		})
	}
}

func ensureTestSlotHotSwapLeaseTTL(ctx context.Context, store ReadStore, preparer TestSlotPreparer, minter RunnerGitHubTokenMinter, project Project, lease Lease) (int, error) {
	updater, ok := store.(LeaseTTLUpdater)
	if !ok || updater == nil {
		return 0, errors.New("test-slot lease TTL updater not configured")
	}
	if lease.State != "claimed" {
		return 0, ErrConflict
	}
	if slotState, hasSlotState := currentTestSlotState(ctx, store, lease); hasSlotState && slotState == SlotStateCleaning {
		return 0, ErrConflict
	}
	expiresAt := testSlotLeaseExpiresAt(lease)
	if expiresAt == nil {
		return 0, nil
	}
	now := time.Now().UTC()
	if !expiresAt.After(now) {
		return 0, ErrConflict
	}
	minRemainingSeconds := hotSwapMinTTLForTestLease(ctx, store, project)
	if minRemainingSeconds <= 0 {
		return 0, nil
	}
	targetExpiresAt := now.Add(time.Duration(minRemainingSeconds) * time.Second)
	if !targetExpiresAt.After(*expiresAt) {
		return 0, nil
	}
	started := lease.RequestedAt
	if lease.AssignedAt != nil {
		started = *lease.AssignedAt
	}
	if started.IsZero() {
		return 0, ErrConflict
	}
	newTTLSeconds := int(targetExpiresAt.Sub(started) / time.Second)
	if started.Add(time.Duration(newTTLSeconds) * time.Second).Before(targetExpiresAt) {
		newTTLSeconds++
	}
	if newTTLSeconds <= lease.TTLSeconds {
		return 0, nil
	}
	ref := LeasePublicRefFromLease(lease)
	updated, err := updater.UpdateLeaseTTLByRef(ctx, lease.Project, ref, newTTLSeconds)
	if err != nil {
		return 0, err
	}
	rearmUpdatedLeaseTimer(ctx, store, preparer, minter, updated)
	return updated.TTLSeconds - lease.TTLSeconds, nil
}

// testSlotDeployImageValueKey is the chart value the deploy override pins to the
// verified commit's CI image (helm --set <key>=<resolved image>). It defaults to "image.tag"
// — the universal convention across these charts and the exact key the
// chart-image-tag drift fix (glimmung#622) standardized on — so the common
// project needs no per-app config. A project whose chart names its image value
// differently overrides it via metadata `test_slot_deploy.image_value_key`.
//
// This is a *dynamic per-deploy* override (always the commit under test), set at
// deploy time rather than pinned in test_slot_helm.values, so it is not the
// retired static metadata pin that drift fix deleted and does not reintroduce
// the slot-staleness surface that the test_slot_helm guard protects against.
func testSlotDeployImageValueKey(project Project) string {
	for _, key := range []string{"test_slot_deploy", "testSlotDeploy"} {
		if raw, ok := mapFromMap(project.Metadata, key); ok {
			if value := configString(raw, "image_value_key", "imageValueKey"); value != "" {
				return value
			}
		}
	}
	return "image.tag"
}

func testSlotDeployImageOverrideValue(image ResolvedTestSlotImage, imageValueKey string) string {
	if testSlotDeployImageValueKeyWantsTag(imageValueKey) {
		return image.Tag
	}
	return image.Image
}

func testSlotDeployImageValueKeyWantsTag(imageValueKey string) bool {
	key := strings.TrimSpace(imageValueKey)
	if key == "" {
		return false
	}
	normalized := strings.ToLower(strings.NewReplacer("_", ".", "-", ".").Replace(key))
	return normalized == "tag" || strings.HasSuffix(normalized, ".tag") || strings.HasSuffix(normalized, "imagetag")
}

// githubResolveSHA resolves a git ref (branch, tag, or SHA) to its commit SHA via
// the GitHub commits API. The live wiring for the deploy endpoint's refResolver.
func githubResolveSHA(ctx context.Context, httpClient *http.Client, slug, ref, token string) (string, error) {
	var payload struct {
		SHA string `json:"sha"`
	}
	apiURL := githubAPIBase + "/repos/" + slug + "/commits/" + url.PathEscape(ref)
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.SHA) == "" {
		return "", fmt.Errorf("no commit sha for ref %q in %s", ref, slug)
	}
	return payload.SHA, nil
}
