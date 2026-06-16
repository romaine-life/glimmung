package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/domain/hotswap"
)

// applyHotSwapTimeoutDefault is the server-side default when the caller
// doesn't specify timeout_seconds. The build-and-swap operation should
// complete in 30-90s in practice; 120s gives buffer for network jitter
// and slow image pulls on the first cold start.
const applyHotSwapTimeoutDefault = 120 * time.Second

// applyHotSwapTimeoutMax is the hard server cap. The caller can ask for
// less; they can't ask for more. This keeps a single bad caller from
// holding a request open indefinitely.
const applyHotSwapTimeoutMax = 600 * time.Second

// applyHotSwapPerformer is the function-typed seam the test harness uses to
// inject a stub. Production wires this to DispatchHotSwap with a real
// httpK8sJobClient. It submits the Job and returns a "running" result with the
// job handle; it does NOT wait for completion (the gated finalizer does).
type applyHotSwapPerformer func(ctx context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error)

// hotSwapDiffResolver is the function-typed seam for computing the classifier's
// diff context. Production wires it to resolveHotSwapDiff (a live GitHub Compare
// call); tests pass nil to skip the network or a stub to assert plumbing.
type hotSwapDiffResolver func(ctx context.Context, slug, baseRef, headRef, token string) (hotSwapDiff, error)

type TestSlotApplyHotSwapRequest struct {
	Project          string  `json:"project"`
	SlotIndex        *int    `json:"slot_index,omitempty"`
	SlotName         *string `json:"slot_name,omitempty"`
	ArtifactKind     string  `json:"artifact_kind"`
	GitRef           string  `json:"git_ref"`
	ValidationTarget string  `json:"validation_target,omitempty"`
	TimeoutSeconds   *int    `json:"timeout_seconds,omitempty"`
	// BaseRef is the diff base for the fidelity classifier. When empty,
	// glimmung resolves the repository's default branch. The classifier's
	// changed-file set is computed as base...git_ref.
	BaseRef string `json:"base_ref,omitempty"`
}

type TestSlotApplyHotSwapResult struct {
	Lease          string                         `json:"lease"`
	Apply          ApplyHotSwapResult             `json:"apply"`
	Entry          TestSlotHotSwapHistoryEntry    `json:"history_entry"`
	LeaseExtension *TestSlotHotSwapLeaseExtension `json:"lease_extension,omitempty"`
}

// applyTestSlotHotSwap is the developer-driven build-and-swap endpoint.
// It is asynchronous-with-poll: the POST dispatches the build-and-swap Job and
// returns immediately with a "running" result + job handle; the gated
// apply-hot-swap finalizer records the terminal outcome when the Job completes;
// the caller polls getApplyHotSwapStatus until the entry is terminal. No single
// HTTP request is held open for the build, so timeout_seconds is honored by the
// caller's poll loop and the durable outcome survives client disconnects,
// proxy deadlines, and orchestrator rollouts. The earlier synchronous design
// (one request blocked for the whole build) tied the result and the history
// write to the inbound connection — a ~30s proxy deadline aborted both.
//
// Caller flow:
//
//  1. POST { project, slot_index|slot_name, artifact_kind, git_ref, validation_target, timeout_seconds, base_ref? }
//  2. Endpoint resolves the active test-slot lease for project+slot.
//  3. Endpoint reads the project's hot-swap contract from metadata.
//  4. Endpoint validates artifact_kind is supported (static, backend, or the
//     runner artifacts agent_runner, codex_runner, antigravity_runner) and the
//     kind's request-time fields are present.
//  5. Endpoint resolves the classifier diff context (base...git_ref) and
//     dispatches a build-and-swap Job via DispatchHotSwap (timeout enforced as
//     the Job's activeDeadlineSeconds), then returns without waiting.
//  6. Endpoint appends an initial "running" hot-swap history entry carrying the
//     job handle — the breadcrumb the finalizer and the status poll join on.
//  7. Endpoint extends the lease so the slot survives the full build-and-swap.
//  8. Endpoint returns the structured result with status "running".
//
// Hot-swap history is the durable state — it lives in the system, not in the
// request body. A caller that disconnects can re-query the lease history (or
// the status endpoint) to see the terminal result the finalizer records.
func applyTestSlotHotSwap(store ReadStore, preparer TestSlotPreparer, minter RunnerGitHubTokenMinter, performer applyHotSwapPerformer, resolveDiff hotSwapDiffResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(TestSlotHotSwapHistoryStore)
		stateStore, hasState := store.(StateStore)
		if !ok || writer == nil || !hasState || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot hot-swap history store not configured")
			return
		}
		var req TestSlotApplyHotSwapRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Project = strings.TrimSpace(req.Project)
		req.ArtifactKind = strings.TrimSpace(req.ArtifactKind)
		req.GitRef = strings.TrimSpace(req.GitRef)
		req.ValidationTarget = strings.TrimSpace(req.ValidationTarget)
		if req.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if req.ArtifactKind == "" {
			writeProblem(w, http.StatusBadRequest, "artifact_kind required")
			return
		}
		if req.GitRef == "" {
			writeProblem(w, http.StatusBadRequest, "git_ref required")
			return
		}

		// Timeout: clamp caller-requested to [1s, applyHotSwapTimeoutMax];
		// default to applyHotSwapTimeoutDefault when unset. The clamping
		// happens here (server-side) so a caller asking for "8 hours"
		// can't hold a connection open beyond the hard cap.
		timeout := applyHotSwapTimeoutDefault
		if req.TimeoutSeconds != nil {
			if *req.TimeoutSeconds <= 0 {
				timeout = applyHotSwapTimeoutDefault
			} else if time.Duration(*req.TimeoutSeconds)*time.Second > applyHotSwapTimeoutMax {
				timeout = applyHotSwapTimeoutMax
			} else {
				timeout = time.Duration(*req.TimeoutSeconds) * time.Second
			}
		}

		// Resolve lease. Reuse the existing helper so the slot-index/name
		// resolution mirrors the record-history endpoint.
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
		slotName, _ := stringFromMap(lease.Metadata, "runner_slot_name")
		if strings.TrimSpace(slotName) == "" {
			writeProblem(w, http.StatusBadRequest, "lease has no runner_slot_name (cannot derive target namespace)")
			return
		}

		// Resolve the project + contract. ListProjects matches by name +
		// limit=10 so a small typo doesn't fail silently (the historic
		// pattern in record-history); we filter for an exact name match.
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
		contract, ok, err := hotswap.FromMetadata(project.Metadata)
		if err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid hot-swap contract: "+err.Error())
			return
		}
		if !ok || !contract.Enabled {
			writeProblem(w, http.StatusUnprocessableEntity, "project has no enabled test_slot_hot_swap contract")
			return
		}
		if contract.FidelityClassifier.Enabled {
			if req.ValidationTarget == "" {
				writeProblem(w, http.StatusUnprocessableEntity, "validation_target required when test_slot_hot_swap.fidelity_classifier is enabled")
				return
			}
		}
		validationTarget, err := normalizeHotSwapValidationTarget(req.ValidationTarget)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}

		// Backend's apply-endpoint fields (builder_image, pod_selector,
		// container, health_port) are optional at Validate time so contracts
		// registered before backend joined the apply endpoint still parse;
		// the endpoint enforces them here at request time, mirroring static
		// below. health_port is required because the swap is health-gated:
		// the swap container polls http://127.0.0.1:<health_port><health_path>
		// inside the pod to confirm the re-exec actually serves.
		if req.ArtifactKind == "backend" {
			missing := ""
			switch {
			case strings.TrimSpace(contract.Backend.BuilderImage) == "":
				missing = "builder_image"
			case strings.TrimSpace(contract.Backend.PodSelector) == "":
				missing = "pod_selector"
			case strings.TrimSpace(contract.Backend.Container) == "":
				missing = "container"
			case contract.Backend.HealthPort <= 0:
				missing = "health_port"
			}
			if missing != "" {
				writeProblem(w, http.StatusUnprocessableEntity, "contract.backend."+missing+" required for apply endpoint")
				return
			}
		}
		// Static's builder_image/pod_selector/container are optional at
		// Validate time (contracts registered before static gained these
		// fields must still re-register), so the apply endpoint enforces
		// them at request time, mirroring backend.builder_image above.
		if req.ArtifactKind == "static" {
			missing := ""
			switch {
			case strings.TrimSpace(contract.Static.BuilderImage) == "":
				missing = "builder_image"
			case strings.TrimSpace(contract.Static.PodSelector) == "":
				missing = "pod_selector"
			case strings.TrimSpace(contract.Static.Container) == "":
				missing = "container"
			}
			if missing != "" {
				writeProblem(w, http.StatusUnprocessableEntity, "contract.static."+missing+" required for apply endpoint")
				return
			}
		}

		// Target namespace convention. Runner artifacts live in session
		// pods (`<slot_name>-sessions`); static assets and the backend
		// binary live in the slot's app pods (`<slot_name>`). Both
		// namespaces carry the glimmung test-slot labels. If a future
		// project needs a different namespace, extend the contract; for v1
		// the convention is sufficient.
		targetNamespace := slotName + "-sessions"
		if req.ArtifactKind == "static" || req.ArtifactKind == "backend" {
			targetNamespace = slotName
		}

		// RepoURL + clone auth. The build Job clones a (typically private) repo,
		// so it needs a token. Mirror the runner-launch path: mint a short-lived
		// installation token and pass it to the Job, with the URL carrying only
		// the x-access-token username — the token reaches git via GIT_ASKPASS in
		// the build script, never on a command line or in the build logs.
		repoURL := ""
		repoToken := ""
		if slug := strings.TrimSpace(project.GitHubRepo); slug != "" {
			repoURL = "https://github.com/" + slug + ".git"
			if minter != nil {
				tok, err := minter.RepositoryInstallationToken(r.Context(), slug, map[string]string{"contents": "read"})
				if err != nil {
					writeInternalError(w, r, err, "mint clone token for hot-swap: "+err.Error())
					return
				}
				repoToken = tok
				repoURL = "https://x-access-token@github.com/" + slug + ".git"
			}
		}

		// Pod selector: the contract's <runner>.pod_selector flows into the
		// Job's swap script, which resolves target pods at run time via kubectl
		// inside the alpine/k8s container — no kubectl needed in the glimmung pod.

		// Diff context for the fidelity classifier. The build Job's shallow
		// single-SHA checkout cannot compute a real diff, so glimmung resolves
		// the changed-file set here (GitHub Compare API, merge-base three-dot)
		// and passes it down. Best-effort: a failure is recorded in diagnostics
		// but does not block the swap — the in-container classifier still runs.
		var diff hotSwapDiff
		var diffErr error
		if resolveDiff != nil {
			if slug := strings.TrimSpace(project.GitHubRepo); slug != "" && repoToken != "" {
				diff, diffErr = resolveDiff(r.Context(), slug, req.BaseRef, req.GitRef, repoToken)
			}
		}

		// Dispatch on a context detached from the inbound request: a client that
		// disconnects mid-call must not abort the Job submission or the durable
		// initial-history write. The build-and-swap itself runs as a Kubernetes
		// Job (deadline enforced via activeDeadlineSeconds) and is finalized by
		// the gated apply-hot-swap watcher — fully decoupled from this request.
		bgCtx := context.WithoutCancel(r.Context())

		applyResult, _ := performer(bgCtx, ApplyHotSwapOptions{
			Project:          req.Project,
			ArtifactKind:     req.ArtifactKind,
			GitRef:           req.GitRef,
			RepoURL:          repoURL,
			RepoToken:        repoToken,
			TargetNamespace:  targetNamespace,
			SlotName:         slotName,
			ValidationTarget: validationTarget,
			Contract:         contract,
			Timeout:          timeout,
			BaseRef:          diff.BaseRef,
			HeadRef:          req.GitRef,
			ChangedFiles:     diff.ChangedFiles,
		})

		// Record the dispatch outcome. On a successful dispatch this is the
		// "running" breadcrumb the caller polls against; the gated finalizer
		// later appends the terminal entry. If the dispatch itself failed (the
		// Job never reached the apiserver) the status is already terminal
		// ("swap_failed") and no finalizer will touch it.
		status := applyResult.Outcome
		if status == "" {
			status = "swap_failed"
		}
		summary := fmt.Sprintf("apply_hot_swap dispatched kind=%s git_ref=%s validation_target=%s job=%s status=%s", req.ArtifactKind, req.GitRef, validationTarget, applyResult.JobName, status)
		diagnostics := map[string]any{
			"job_name":          applyResult.JobName,
			"job_namespace":     applyResult.JobNamespace,
			"slot_name":         slotName,
			"validation_target": validationTarget,
			"base_ref":          diff.BaseRef,
			"head_ref":          req.GitRef,
			"changed_files":     len(diff.ChangedFiles),
		}
		if diff.Truncated {
			diagnostics["changed_files_truncated"] = true
		}
		if diffErr != nil {
			diagnostics["changed_files_error"] = diffErr.Error()
		}
		if applyResult.Error != "" {
			diagnostics["error"] = applyResult.Error
		}
		entry := TestSlotHotSwapHistoryEntry{
			Operation:   "apply_hot_swap",
			Status:      status,
			Summary:     summary,
			Diagnostics: diagnostics,
			Timings:     applyResult.Timings,
			CreatedAt:   time.Now().UTC(),
		}
		// The initial-history write uses the detached context so a disconnected
		// caller still leaves a durable breadcrumb the finalizer (and a
		// re-query) can attach to.
		leaseWithHistory, histErr := writer.AppendTestSlotHotSwapHistory(bgCtx, req.Project, leaseRef, entry)
		if histErr != nil {
			diagnostics["history_write_error"] = histErr.Error()
		} else {
			lease = leaseWithHistory
		}

		// Extend the lease now so the slot survives the full build-and-swap
		// (the build alone can run ~90s); the finalizer re-checks the minimum
		// when the Job reaches its terminal condition.
		leaseExtension, extendErr := ensureHotSwapLeaseMinimum(bgCtx, store, preparer, minter, project, lease)
		if extendErr != nil {
			diagnostics["lease_extension_error"] = extendErr.Error()
		}

		writeJSON(w, http.StatusOK, TestSlotApplyHotSwapResult{
			Lease:          leaseRef,
			Apply:          applyResult,
			Entry:          entry,
			LeaseExtension: leaseExtension,
		})
	}
}

func normalizeHotSwapValidationTarget(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "":
		return "existing_session", nil
	case "existing_pod", "existing_pods", "existing_session", "existing_sessions":
		return "existing_session", nil
	case "new_session", "future_session", "future_sessions":
		return "new_session", nil
	case "full_runtime", "branch_image":
		return "full_runtime", nil
	default:
		return "", fmt.Errorf("validation_target %q is not supported (use existing_session, new_session, or full_runtime)", strings.TrimSpace(value))
	}
}
