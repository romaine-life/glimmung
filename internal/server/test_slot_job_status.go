package server

import (
	"net/http"
	"strings"
)

// TestSlotJobStatusResult is the poll response: the durable slot operation
// history entry for a dispatched job. Status is "running" until the deploy
// worker records a terminal outcome.
type TestSlotJobStatusResult struct {
	Lease  string                  `json:"lease"`
	Job    string                  `json:"job_name"`
	Status string                  `json:"status"`
	Entry  *TestSlotOpHistoryEntry `json:"history_entry,omitempty"`
}

// getTestSlotJobStatus serves GET /v1/test-slots/jobs/{project}/{job}. It reads
// durable lease operation history and returns the latest entry for the job. This
// is the poll surface the MCP wrapper drives to turn a non-blocking deploy into
// a synchronous result without holding any long request open.
func getTestSlotJobStatus(store ReadStore) http.HandlerFunc {
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
			if entry, ok := latestSlotOpEntryForJob(lease, jobName); ok {
				e := entry
				writeJSON(w, http.StatusOK, TestSlotJobStatusResult{
					Lease:  LeasePublicRefFromLease(lease),
					Job:    jobName,
					Status: entry.Status,
					Entry:  &e,
				})
				return
			}
		}
		writeProblem(w, http.StatusNotFound, "no slot operation history for job "+jobName+" in project "+project)
	}
}
