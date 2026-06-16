package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/metrics"
)

// applyHotSwapJobNamespace is where apply-hot-swap Jobs are dispatched and the
// namespace the finalizer's recovery list scopes its work to. It mirrors
// DispatchHotSwap's JobNamespace default.
const applyHotSwapJobNamespace = "glimmung-runs"

// shouldStartApplyHotSwapJobWatcher gates the finalizer on the control-plane
// switch and the required store capability. Factored out so the gate is unit
// testable per the Test Slots contract: "a new background reconciler or
// recovery sweep adds a test that proves the Start… function is unreachable
// when Settings.ControlPlaneLoopsEnabled is false."
func shouldStartApplyHotSwapJobWatcher(settings Settings, store ReadStore) bool {
	if !settings.ControlPlaneLoopsEnabled {
		return false
	}
	if store == nil {
		return false
	}
	if _, ok := any(store).(StateStore); !ok {
		return false
	}
	_, ok := any(store).(TestSlotHotSwapHistoryStore)
	return ok
}

// StartApplyHotSwapJobWatcher launches the gated, event-driven finalizer for
// apply-hot-swap Jobs. It is a no-op unless the control plane is enabled and
// the store records hot-swap history — the same isolation gate the run-job
// watchers and reconcilers use, so a slot process never finalizes prod Jobs.
//
// The finalizer reuses the cluster-wide k8sJobWatcher machinery (list + watch +
// backoff): for each terminal apply-hot-swap Job the apiserver pushes, it
// collects logs, classifies the outcome, appends the terminal hot-swap history
// entry (idempotently), re-extends the lease, and deletes the Job — all on its
// own background context, so the durable outcome never depends on the
// dispatching request staying connected. listAndSync at startup re-finalizes
// any Job that completed while glimmung was down.
func StartApplyHotSwapJobWatcher(ctx context.Context, settings Settings, store ReadStore, preparer TestSlotPreparer, minter RunnerGitHubTokenMinter, logf func(string, ...any)) {
	if !shouldStartApplyHotSwapJobWatcher(settings, store) {
		return
	}
	history, _ := any(store).(TestSlotHotSwapHistoryStore)
	state, _ := any(store).(StateStore)
	deps := watcherDeps{
		settings:        settings,
		namespace:       applyHotSwapJobNamespace,
		logf:            logf,
		hotSwapHistory:  history,
		hotSwapState:    state,
		hotSwapPreparer: preparer,
		hotSwapMinter:   minter,
		hotSwapK8s:      newHTTPK8sJobClient(settings),
	}
	go (&k8sJobWatcher{watcherDeps: deps, labelSelector: watchHotSwapSelector}).run(ctx)
}

// dispatchHotSwapTerminal finalizes one terminal apply-hot-swap Job. The shared
// dispatchJobObject only routes here once the Job is terminal, so this never
// waits: it joins the Job back to its leased slot via the slot-name label,
// skips when a terminal entry already exists (idempotent across re-lists and
// duplicate events), classifies the outcome from the terminal status + logs,
// appends the terminal history entry, re-extends the lease, and deletes the Job.
func (w *k8sJobWatcher) dispatchHotSwapTerminal(ctx context.Context, job map[string]any, status RunnerJobStatus, action string) {
	if w.hotSwapHistory == nil || w.hotSwapState == nil || w.hotSwapK8s == nil {
		return
	}
	labels := jobLabels(job)
	project := strings.TrimSpace(labels["glimmung.io/project"])
	slotName := strings.TrimSpace(labels["glimmung.io/slot-name"])
	artifactKind := strings.TrimSpace(labels["glimmung.io/apply-hot-swap-kind"])
	validationTarget := strings.TrimSpace(labels["glimmung.io/hot-swap-validation-target"])
	jobName := jobObjectName(job)
	jobNamespace := jobObjectNamespace(job)
	if jobNamespace == "" {
		jobNamespace = w.namespace
	}
	if project == "" || slotName == "" || jobName == "" {
		w.log("apply-hot-swap finalize: terminal job missing labels project=%q slot=%q job=%q", project, slotName, jobName)
		return
	}

	leases, err := w.hotSwapState.ListLeases(ctx)
	if err != nil {
		w.log("apply-hot-swap finalize: list leases failed job=%s: %v", jobName, err)
		return
	}
	lease, ok := findActiveCheckoutLeaseBySlot(leases, project, slotName)
	if !ok {
		w.log("apply-hot-swap finalize: no checkout lease for project=%s slot=%s job=%s", project, slotName, jobName)
		return
	}
	if hotSwapJobHasTerminalEntry(lease, jobName) {
		// Already finalized — make sure the Job is cleaned up and return.
		_ = w.hotSwapK8s.DeleteJob(ctx, jobNamespace, jobName)
		return
	}

	succeeded := status.IsTerminallySucceeded() && !status.IsTerminallyFailed()
	result := finalizeHotSwap(ctx, w.hotSwapK8s, finalizeHotSwapInputs{
		JobName:          jobName,
		JobNamespace:     jobNamespace,
		ArtifactKind:     artifactKind,
		ValidationTarget: validationTarget,
	}, succeeded, status.FailureReason())

	metrics.RecordHotSwap(result.Outcome, hotSwapJobDuration(job, status))

	diagnostics := map[string]any{
		"job_name":          jobName,
		"job_namespace":     jobNamespace,
		"slot_name":         slotName,
		"validation_target": validationTarget,
		"build_logs_tail":   result.BuildLogsTail,
		"swap_logs_tail":    result.SwapLogsTail,
	}
	if result.Error != "" {
		diagnostics["error"] = result.Error
	}
	entry := TestSlotHotSwapHistoryEntry{
		Operation:   "apply_hot_swap",
		Status:      result.Outcome,
		Summary:     fmt.Sprintf("apply_hot_swap finalized kind=%s validation_target=%s job=%s outcome=%s", artifactKind, validationTarget, jobName, result.Outcome),
		Diagnostics: diagnostics,
		Timings:     result.Timings,
		CreatedAt:   time.Now().UTC(),
	}
	leaseRef := LeasePublicRefFromLease(lease)
	updatedLease, histErr := w.hotSwapHistory.AppendTestSlotHotSwapHistory(ctx, project, leaseRef, entry)
	if histErr != nil {
		w.log("apply-hot-swap finalize: history append failed job=%s: %v", jobName, histErr)
		return
	}
	if _, err := ensureHotSwapLeaseMinimumByProjectName(ctx, w.hotSwapState, w.hotSwapPreparer, w.hotSwapMinter, project, updatedLease); err != nil {
		w.log("apply-hot-swap finalize: lease extension failed job=%s: %v", jobName, err)
	}
	_ = w.hotSwapK8s.DeleteJob(ctx, jobNamespace, jobName)
	metrics.RecordRunWatchEvent("hotswap", "synthesized_"+action)
}

// findActiveCheckoutLeaseBySlot resolves the live test-slot checkout lease for
// a project + runner slot name. It prefers a claimed lease; failing that it
// returns any checkout lease for the slot so a terminal Job can still be
// recorded against a just-released lease's history.
func findActiveCheckoutLeaseBySlot(leases []Lease, project, slotName string) (Lease, bool) {
	var fallback Lease
	haveFallback := false
	for _, l := range leases {
		if l.Project != project || !boolFromMap(l.Metadata, "test_slot_checkout") {
			continue
		}
		name, _ := stringFromMap(l.Metadata, "runner_slot_name")
		if strings.TrimSpace(name) != slotName {
			continue
		}
		if l.State == "claimed" {
			return l, true
		}
		if !haveFallback {
			fallback = l
			haveFallback = true
		}
	}
	return fallback, haveFallback
}

// hotSwapJobDuration derives the build-and-swap duration from the Job's
// start/terminal timestamps (best-effort; 0 when unavailable).
func hotSwapJobDuration(job map[string]any, status RunnerJobStatus) time.Duration {
	start := hotSwapJobStartTime(job)
	end := status.TerminalTime()
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func hotSwapJobStartTime(job map[string]any) time.Time {
	st, _ := job["status"].(map[string]any)
	if st == nil {
		return time.Time{}
	}
	raw, _ := st["startTime"].(string)
	if strings.TrimSpace(raw) == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
