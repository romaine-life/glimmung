package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/romaine-life/glimmung/internal/metrics"
)

// RunnerStore handles runner k8s_job runner event recording and status.
type RunnerStore interface {
	// GetRunnerStatusByID returns the run state for the runner.
	GetRunnerStatusByID(ctx context.Context, project, runID string) (RunnerStatusResponse, error)
	// RecordRunnerEventByID writes one idempotent runner event. The store resolves attempt context from the run doc.
	RecordRunnerEventByID(ctx context.Context, project, runID string, req RunnerEventRequest) (RunnerEventResult, error)
	// ListRunnerEventsByID returns ordered runner events for a run with optional filters.
	ListRunnerEventsByID(ctx context.Context, project, runID string, attemptIndex *int, jobID *string, stepSlug *string, afterSeq *int, limit *int) (RunnerLogsResponse, error)
}

// RunnerEventRequest is the body for POST /run/events.
type RunnerEventRequest struct {
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	AttemptIndex *int           `json:"attempt_index,omitempty"`
	StepSlug     *string        `json:"step_slug"`
	Message      *string        `json:"message"`
	ExitCode     *int           `json:"exit_code"`
	Metadata     map[string]any `json:"metadata"`
}

// RunnerEventResult is the response for POST /run/events.
type RunnerEventResult struct {
	RunRef   string `json:"run_ref"`
	JobID    string `json:"job_id"`
	Seq      int    `json:"seq"`
	Accepted bool   `json:"accepted"`
}

// RunnerStatusResponse is the response for GET /run/status.
type RunnerStatusResponse struct {
	Project           string     `json:"project"`
	RunRef            string     `json:"run_ref"`
	State             string     `json:"state"`
	AttemptIndex      int        `json:"attempt_index"`
	CancelRequested   bool       `json:"cancel_requested"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at"`
	CancelReason      *string    `json:"cancel_reason"`
}

// RunnerLogEvent is one event record in a runner run log stream.
type RunnerLogEvent struct {
	Project      string         `json:"project"`
	RunRef       string         `json:"run_ref"`
	AttemptIndex int            `json:"attempt_index"`
	Phase        string         `json:"phase"`
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	StepSlug     string         `json:"step_slug"`
	Message      string         `json:"message"`
	ExitCode     *int           `json:"exit_code"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    string         `json:"created_at"`
}

// RunnerLogsResponse is the response for GET /run/events.
type RunnerLogsResponse struct {
	Project      string           `json:"project"`
	RunRef       string           `json:"run_ref"`
	AttemptIndex *int             `json:"attempt_index"`
	JobID        *string          `json:"job_id"`
	Events       []RunnerLogEvent `json:"events"`
	ArchiveURL   *string          `json:"archive_url"`
}

// runnerEventsByNumber handles GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/run/events
func runnerEventsByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runnerStore, mutStore, ok := requireRunnerStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		resp, err := runnerStore.ListRunnerEventsByID(r.Context(), project, runID,
			parseOptionalIntQuery(r, "attempt_index"),
			parseOptionalStringQuery(r, "job_id"),
			parseOptionalStringQuery(r, "step_slug"),
			parseOptionalIntQuery(r, "after_seq"),
			parseOptionalIntQuery(r, "limit"),
		)
		writeRunnerLogsOrError(w, r, resp, err)
	}
}

// runnerEventWriteByNumber handles POST /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/run/events
func runnerEventWriteByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runnerStore, mutStore, ok := requireRunnerStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		postRunnerEvent(w, r, runnerStore, project, runID)
	}
}

// runnerEventWriteByCallbackToken handles POST /v1/run-callbacks/{callback_token}/run/events
func runnerEventWriteByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runnerStore, mutStore, ok := requireRunnerStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByCallbackToken(w, r, mutStore)
		if !ok {
			return
		}
		postRunnerEvent(w, r, runnerStore, project, runID)
	}
}

// runnerStatusByNumber handles GET /v1/projects/{project}/issues/{issue_number}/runs/{run_number}/run/status
func runnerStatusByNumber(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runnerStore, mutStore, ok := requireRunnerStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByNumber(w, r, mutStore)
		if !ok {
			return
		}
		getRunnerStatus(w, r, runnerStore, project, runID)
	}
}

// runnerStatusByCallbackToken handles GET /v1/run-callbacks/{callback_token}/run/status
func runnerStatusByCallbackToken(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runnerStore, mutStore, ok := requireRunnerStores(w, store)
		if !ok {
			return
		}
		runID, project, ok := resolveRunByCallbackToken(w, r, mutStore)
		if !ok {
			return
		}
		getRunnerStatus(w, r, runnerStore, project, runID)
	}
}

// --- shared helpers ---

func requireRunnerStores(w http.ResponseWriter, store ReadStore) (RunnerStore, RunMutationStore, bool) {
	runnerStore, ok := store.(RunnerStore)
	if !ok || runnerStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runner run store not configured")
		return nil, nil, false
	}
	mutStore, ok := store.(RunMutationStore)
	if !ok || mutStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
		return nil, nil, false
	}
	return runnerStore, mutStore, true
}

func resolveRunByNumber(w http.ResponseWriter, r *http.Request, mutStore RunMutationStore) (runID, project string, ok bool) {
	project = r.PathValue("project")
	issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
	if err != nil || issueNumber < 1 {
		writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
		return "", "", false
	}
	runID, _, err = mutStore.ReadRunIDForNumber(r.Context(), project, issueNumber, r.PathValue("run_number"))
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return "", "", false
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return "", "", false
	}
	return runID, project, true
}

func resolveRunByCallbackToken(w http.ResponseWriter, r *http.Request, mutStore RunMutationStore) (runID, project string, ok bool) {
	token := r.PathValue("callback_token")
	var err error
	runID, project, _, err = mutStore.ReadRunIDForCallbackToken(r.Context(), token)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run callback token not found")
		return "", "", false
	}
	if err != nil {
		writeInternalError(w, r, err, "read run by callback token failed")
		return "", "", false
	}
	return runID, project, true
}

func postRunnerEvent(w http.ResponseWriter, r *http.Request, store RunnerStore, project, runID string) {
	var req RunnerEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.JobID == "" {
		writeProblem(w, http.StatusBadRequest, "job_id required")
		return
	}
	if req.Seq < 1 {
		writeProblem(w, http.StatusBadRequest, "seq must be >= 1")
		return
	}
	if req.Event == "" {
		writeProblem(w, http.StatusBadRequest, "event required")
		return
	}
	result, err := store.RecordRunnerEventByID(r.Context(), project, runID, req)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if errors.Is(err, ErrConflict) {
		writeProblem(w, http.StatusConflict, "duplicate event with different payload")
		return
	}
	var validationErr ValidationError
	if errors.As(err, &validationErr) {
		writeProblem(w, http.StatusBadRequest, validationErr.Message)
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "record runner event failed")
		return
	}
	// Bounded-cardinality metric for inner-Job registrations
	// (docs/inner-job-observation.md). The intent label has already
	// been normalised by the runner against the closed enum in
	// innerjob.Parse, so safeLabel in the metrics package only catches
	// the empty-string case.
	if req.Event == "inner_job_registered" {
		intent, _ := req.Metadata["intent"].(string)
		metrics.RecordRunInnerJobRegistered(intent)
	}
	writeJSON(w, http.StatusOK, result)
}

func getRunnerStatus(w http.ResponseWriter, r *http.Request, store RunnerStore, project, runID string) {
	resp, err := store.GetRunnerStatusByID(r.Context(), project, runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if errors.Is(err, ErrConflict) {
		writeProblem(w, http.StatusConflict, "latest attempt is not a runner k8s_job")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "get runner run status failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeRunnerLogsOrError(w http.ResponseWriter, r *http.Request, resp RunnerLogsResponse, err error) {
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "list runner events failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseOptionalIntQuery parses an optional integer query parameter, returning nil if absent or invalid.
func parseOptionalIntQuery(r *http.Request, name string) *int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &v
}

// parseOptionalStringQuery returns a non-empty query parameter value, or nil.
func parseOptionalStringQuery(r *http.Request, name string) *string {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil
	}
	return &raw
}
