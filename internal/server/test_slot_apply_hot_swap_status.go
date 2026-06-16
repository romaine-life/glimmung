package server

import (
	"net/http"
	"strings"
)

// TestSlotApplyHotSwapStatusResult is the poll response: the durable hot-swap
// history entry for a dispatched job. Status is "running" until the gated
// finalizer records a terminal outcome (persisted | build_failed | swap_failed
// | timeout).
type TestSlotApplyHotSwapStatusResult struct {
	Lease  string                       `json:"lease"`
	Job    string                       `json:"job_name"`
	Status string                       `json:"status"`
	Entry  *TestSlotHotSwapHistoryEntry `json:"history_entry,omitempty"`
}

// getApplyHotSwapStatus serves GET /v1/test-slots/apply-hot-swap/{project}/{job}.
// It reads the durable lease history — the same record the dispatching POST
// wrote and the gated finalizer updates — and returns the latest entry for the
// job. This is the poll surface the MCP wrapper drives to turn a non-blocking
// dispatch into a synchronous result without holding any long request open.
func getApplyHotSwapStatus(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stateStore, ok := store.(StateStore)
		if !ok || stateStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "test-slot state store not configured")
			return
		}
		project := strings.TrimSpace(r.PathValue("project"))
		jobName := strings.TrimSpace(r.PathValue("job"))
		if project == "" || jobName == "" {
			writeProblem(w, http.StatusBadRequest, "project and job are required")
			return
		}
		leases, err := stateStore.ListLeases(r.Context())
		if err != nil {
			writeInternalError(w, r, err, "list leases: "+err.Error())
			return
		}
		for _, lease := range leases {
			if lease.Project != project {
				continue
			}
			if entry, ok := latestHotSwapEntryForJob(lease, jobName); ok {
				e := entry
				writeJSON(w, http.StatusOK, TestSlotApplyHotSwapStatusResult{
					Lease:  LeasePublicRefFromLease(lease),
					Job:    jobName,
					Status: entry.Status,
					Entry:  &e,
				})
				return
			}
		}
		writeProblem(w, http.StatusNotFound, "no hot-swap history for job "+jobName+" in project "+project)
	}
}
