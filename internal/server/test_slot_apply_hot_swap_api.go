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

// applyHotSwapPerformer is the function-typed seam the test harness
// uses to inject a stub. Production wires this to ApplyHotSwap with a
// real httpK8sJobClient.
type applyHotSwapPerformer func(ctx context.Context, opts ApplyHotSwapOptions) (ApplyHotSwapResult, error)

type TestSlotApplyHotSwapRequest struct {
	Project          string  `json:"project"`
	SlotIndex        *int    `json:"slot_index,omitempty"`
	SlotName         *string `json:"slot_name,omitempty"`
	ArtifactKind     string  `json:"artifact_kind"`
	GitRef           string  `json:"git_ref"`
	ValidationTarget string  `json:"validation_target,omitempty"`
	TimeoutSeconds   *int    `json:"timeout_seconds,omitempty"`
}

type TestSlotApplyHotSwapResult struct {
	Lease          string                         `json:"lease"`
	Apply          ApplyHotSwapResult             `json:"apply"`
	Entry          TestSlotHotSwapHistoryEntry    `json:"history_entry"`
	LeaseExtension *TestSlotHotSwapLeaseExtension `json:"lease_extension,omitempty"`
}

// applyTestSlotHotSwap is the developer-driven build-and-swap endpoint.
// Sync UX per the ArgoCD `app sync` pattern (researched against
// Google AIP-151; ArgoCD is the closer analog for developer-driven k8s
// deploys). Blocks until the dispatched Job completes or the timeout
// elapses. Records hot-swap history on every outcome.
//
// Caller flow:
//
//  1. POST { project, slot_index|slot_name, artifact_kind, git_ref, validation_target, timeout_seconds }
//  2. Endpoint resolves the active test-slot lease for project+slot.
//  3. Endpoint reads the project's hot-swap contract from metadata.
//  4. Endpoint validates artifact_kind is supported (static, backend, or
//     the runner artifacts agent_runner, codex_runner, antigravity_runner)
//     and the kind's request-time fields are present.
//  5. Endpoint dispatches a build-and-swap Job via ops.ApplyHotSwap,
//     blocks on completion.
//  6. Endpoint appends a hot-swap history entry (success or failure).
//  7. Endpoint extends the lease when it has less than the configured
//     hot-swap minimum TTL remaining.
//  8. Endpoint returns the structured result.
//
// Hot-swap history is appended on EVERY outcome — durable state lives
// in the system, not in the request body. A caller that disconnects
// mid-request can re-query the lease history to see the result.
func applyTestSlotHotSwap(store ReadStore, preparer TestSlotPreparer, minter RunnerGitHubTokenMinter, performer applyHotSwapPerformer) http.HandlerFunc {
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

		// Pod selector: the contract's <runner>.pod_selector flows into
		// the Job's swap script, which
		// resolves target pods at run-time via kubectl inside the
		// alpine/k8s container — no kubectl needed in the glimmung
		// pod. (Earlier cut resolved pods up-front in the handler and
		// hit "kubectl: not found" in the glimmung runtime image.)

		ctx := r.Context()
		applyResult, applyErr := performer(ctx, ApplyHotSwapOptions{
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
		})

		// Record hot-swap history on EVERY outcome. The history entry's
		// status mirrors applyResult.Outcome — durable state in the
		// system, regardless of whether the request succeeded.
		status := applyResult.Outcome
		if status == "" {
			status = "swap_failed"
		}
		summary := fmt.Sprintf("apply_hot_swap kind=%s git_ref=%s validation_target=%s outcome=%s", req.ArtifactKind, req.GitRef, validationTarget, status)
		diagnostics := map[string]any{
			"build_logs_tail":   applyResult.BuildLogsTail,
			"swap_logs_tail":    applyResult.SwapLogsTail,
			"validation_target": validationTarget,
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
		leaseWithHistory, histErr := writer.AppendTestSlotHotSwapHistory(ctx, req.Project, leaseRef, entry)
		if histErr != nil {
			// History write failed — log the apply outcome in the body
			// even so. The history failure isn't load-bearing for the
			// caller (they still get the result); it is load-bearing
			// for later operators inspecting the lease. We return 200
			// with the apply result either way.
			diagnostics["history_write_error"] = histErr.Error()
		} else {
			lease = leaseWithHistory
		}

		leaseExtension, extendErr := ensureHotSwapLeaseMinimum(ctx, store, preparer, minter, project, lease)
		if extendErr != nil {
			diagnostics["lease_extension_error"] = extendErr.Error()
		}

		if applyErr != nil {
			// Apply failed — return 200 with the structured result so
			// the caller (MCP tool wrapper) can present the failure
			// cleanly. The Outcome field encodes the failure mode.
			writeJSON(w, http.StatusOK, TestSlotApplyHotSwapResult{
				Lease:          leaseRef,
				Apply:          applyResult,
				Entry:          entry,
				LeaseExtension: leaseExtension,
			})
			return
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
