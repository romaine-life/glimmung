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
// The endpoint resolves only pushed refs, then the SHA→image resolver enforces
// the GitHub gate before any history write or slot mutation: the commit must
// contain current main, belong to an open mergeable PR targeting main, have all
// observed commit checks/statuses green, and have a validated CI lookup image.
func deployImageToTestSlot(store ReadStore, minter RunnerGitHubTokenMinter, performer deployImagePerformer, resolveRef refResolver, resolveImage testSlotImageResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(TestSlotOpHistoryStore)
		stateStore, hasState := store.(StateStore)
		if !ok || writer == nil || !hasState || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot history store not configured")
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

		// Resolve the ref to its commit SHA, then resolve that SHA to the
		// fingerprinted image CI built. Raw SHA tags are not a deploy contract:
		// the image resolver must return a concrete, validated CI image before
		// this endpoint records a running operation or mutates the slot.
		repoToken := ""
		if minter != nil {
			tok, err := minter.RepositoryInstallationToken(r.Context(), slug, map[string]string{"contents": "read", "actions": "read", "pull_requests": "read"})
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
		resolvedImage, err := resolveImage(r.Context(), project, slug, sha, repoToken)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "resolve commit sha to CI image: "+err.Error())
			return
		}
		image := resolvedImage.Image
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
				"job_name":        deployJob,
				"slot_name":       slotName,
				"git_ref":         req.GitRef,
				"sha":             sha,
				"image":           image,
				"image_tag":       resolvedImage.Tag,
				"image_override":  imageOverrideValue,
				"image_source":    resolvedImage.Source,
				"image_value_key": imageValueKey,
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
			diag := map[string]any{"job_name": deployJob, "slot_name": slotName, "git_ref": req.GitRef, "sha": sha, "image": image, "image_tag": resolvedImage.Tag, "image_override": imageOverrideValue, "image_source": resolvedImage.Source}
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
			"lease":          leaseRef,
			"job":            deployJob,
			"status":         "running",
			"git_ref":        req.GitRef,
			"sha":            sha,
			"image":          image,
			"image_tag":      resolvedImage.Tag,
			"image_override": imageOverrideValue,
			"image_source":   resolvedImage.Source,
			"history_entry":  startEntry,
		})
	}
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
