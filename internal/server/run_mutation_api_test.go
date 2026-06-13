package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/auth"
)

// fakeRunMutationStore implements RunMutationStore + RunnerStore for tests.
type fakeRunMutationStore struct {
	fakeReadStore
	runID    string
	runRef   string
	notFound bool

	abortResult AbortRunResult
	abortErr    error

	runnerStatus      RunnerStatusResponse
	runnerStatusErr   error
	runnerEventResult RunnerEventResult
	runnerEventErr    error
	runnerEvents      RunnerLogsResponse
	runnerEventsErr   error
	runnerStepSlug    *string
	runnerAfterSeq    *int
	runnerLimit       *int
}

func (s *fakeRunMutationStore) ReadRunIDForNumber(_ context.Context, project string, issueNumber int, runNumber string) (string, string, error) {
	if s.notFound {
		return "", "", ErrNotFound
	}
	return s.runID, s.runRef, nil
}

func (s *fakeRunMutationStore) ReadRunIDForCallbackToken(_ context.Context, token string) (string, string, string, error) {
	if s.notFound {
		return "", "", "", ErrNotFound
	}
	return s.runID, "myproject", s.runRef, nil
}

func (s *fakeRunMutationStore) AbortRunByID(_ context.Context, project, runID, reason string) (AbortRunResult, error) {
	return s.abortResult, s.abortErr
}

func (s *fakeRunMutationStore) GetRunnerStatusByID(_ context.Context, project, runID string) (RunnerStatusResponse, error) {
	return s.runnerStatus, s.runnerStatusErr
}

func (s *fakeRunMutationStore) RecordRunnerEventByID(_ context.Context, project, runID string, req RunnerEventRequest) (RunnerEventResult, error) {
	return s.runnerEventResult, s.runnerEventErr
}

func (s *fakeRunMutationStore) ListRunnerEventsByID(_ context.Context, project, runID string, attemptIndex *int, jobID *string, stepSlug *string, afterSeq *int, limit *int) (RunnerLogsResponse, error) {
	s.runnerStepSlug = stepSlug
	s.runnerAfterSeq = afterSeq
	s.runnerLimit = limit
	return s.runnerEvents, s.runnerEventsErr
}

func newRunMutHandlerAdmin(store *fakeRunMutationStore) http.Handler {
	return NewWithGitHubClient(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}, nil)
}

func newRunMutHandlerNoAuth(store *fakeRunMutationStore) http.Handler {
	return NewWithGitHubClient(Settings{}, store, nil, nil)
}

// --- abort tests ---

func TestAbortRunByNumber(t *testing.T) {
	runRef := "myproject#42/runs/1.1"
	store := &fakeRunMutationStore{
		runID:       "run-123",
		runRef:      runRef,
		abortResult: AbortRunResult{State: "aborted", RunRef: runRef},
	}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/1.1/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"aborted"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAbortRunByNumberAlreadyTerminal(t *testing.T) {
	runRef := "myproject#42/runs/2.1"
	store := &fakeRunMutationStore{
		runID:       "run-456",
		runRef:      runRef,
		abortResult: AbortRunResult{State: "already_terminal", RunRef: runRef},
	}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/2.1/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"already_terminal"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAbortRunByNumberNotFound(t *testing.T) {
	store := &fakeRunMutationStore{notFound: true}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/99.1/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAbortRunByNumberRequiresAdmin(t *testing.T) {
	store := &fakeRunMutationStore{runID: "x", runRef: "y", abortResult: AbortRunResult{State: "aborted", RunRef: "y"}}
	handler := newRunMutHandlerNoAuth(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/1.1/abort", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 without admin auth, got 200")
	}
}

// TestAbortRunByNumberRejectsNonCanonical guards the agent-facing abort_run
// surface: a bare or ledger run number is rejected before any store lookup.
func TestAbortRunByNumberRejectsNonCanonical(t *testing.T) {
	store := &fakeRunMutationStore{runID: "x", runRef: "y", abortResult: AbortRunResult{State: "aborted", RunRef: "y"}}
	handler := newRunMutHandlerAdmin(store)

	for _, bad := range []string{"1", "9", "6.0"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/42/runs/"+bad+"/abort", nil)
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("run_number=%q: status=%d body=%s (want 400)", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestAbortRunByNumberBadIssueNumber(t *testing.T) {
	store := &fakeRunMutationStore{runID: "x", runRef: "y"}
	handler := newRunMutHandlerAdmin(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/myproject/issues/zero/runs/1/abort", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// --- webhook tests ---

func TestGitHubWebhookNoSecret(t *testing.T) {
	handler := New(Settings{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", strings.NewReader(`{"action":"opened"}`))
	req.Header.Set("x-github-event", "pull_request")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestGitHubWebhookValidSignature(t *testing.T) {
	secret := "webhook-secret"
	body := []byte(`{"action":"created"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	handler := New(Settings{GitHubWebhookSecret: secret})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", bytes.NewReader(body))
	req.Header.Set("x-hub-signature-256", sig)
	req.Header.Set("x-github-event", "push")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookInvalidSignature(t *testing.T) {
	handler := New(Settings{GitHubWebhookSecret: "real-secret"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", strings.NewReader(`{}`))
	req.Header.Set("x-hub-signature-256", "sha256=badhash")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookMissingSignature(t *testing.T) {
	handler := New(Settings{GitHubWebhookSecret: "real-secret"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhook/github", strings.NewReader(`{}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// --- runner run tests ---

func TestRunnerRunStatusByNumber(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-runner",
		runRef: "proj#10/runs/1",
		runnerStatus: RunnerStatusResponse{
			Project:      "proj",
			RunRef:       "proj#10/runs/1",
			State:        "in_progress",
			AttemptIndex: 0,
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/10/runs/1/run/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"in_progress"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRunnerRunStatusByCallbackToken(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-runner",
		runRef: "proj#10/runs/1",
		runnerStatus: RunnerStatusResponse{
			Project:      "proj",
			RunRef:       "proj#10/runs/1",
			State:        "in_progress",
			AttemptIndex: 0,
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/run-callbacks/mytoken/run/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunnerRunStatusNotFoundByNumber(t *testing.T) {
	store := &fakeRunMutationStore{notFound: true}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/10/runs/1/run/status", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunnerRunEventsListByNumber(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-ev",
		runRef: "proj#11/runs/2",
		runnerEvents: RunnerLogsResponse{
			Project: "proj",
			RunRef:  "proj#11/runs/2",
			Events:  []RunnerLogEvent{{JobID: "job1", Seq: 1, Event: "log", Message: "hello"}},
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/11/runs/2/run/events", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"job1"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRunnerRunEventsListByNumberPassesSeqCursor(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-ev",
		runRef: "proj#11/runs/2",
		runnerEvents: RunnerLogsResponse{
			Project: "proj",
			RunRef:  "proj#11/runs/2",
			Events:  []RunnerLogEvent{{JobID: "job1", Seq: 201, Event: "log", Message: "next"}},
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/projects/proj/issues/11/runs/2/run/events?step_slug=run-agent&after_seq=200&limit=200", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runnerAfterSeq == nil || *store.runnerAfterSeq != 200 {
		t.Fatalf("after_seq=%v, want 200", store.runnerAfterSeq)
	}
	if store.runnerStepSlug == nil || *store.runnerStepSlug != "run-agent" {
		t.Fatalf("step_slug=%v, want run-agent", store.runnerStepSlug)
	}
	if store.runnerLimit == nil || *store.runnerLimit != 200 {
		t.Fatalf("limit=%v, want 200", store.runnerLimit)
	}
}

func TestRunnerRunEventWriteByCallbackToken(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:  "run-ev",
		runRef: "proj#11/runs/2",
		runnerStatus: RunnerStatusResponse{
			Project:      "proj",
			RunRef:       "proj#11/runs/2",
			State:        "in_progress",
			AttemptIndex: 0,
		},
		runnerEventResult: RunnerEventResult{
			RunRef:   "proj#11/runs/2",
			JobID:    "myjob",
			Seq:      5,
			Accepted: true,
		},
	}
	handler := newRunMutHandlerNoAuth(store)

	body, _ := json.Marshal(RunnerEventRequest{JobID: "myjob", Seq: 5, Event: "log"})
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/tok/run/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRunnerRunEventWriteByNumberValidation(t *testing.T) {
	store := &fakeRunMutationStore{
		runID:             "run-ev",
		runRef:            "proj#11/runs/2",
		runnerStatus:      RunnerStatusResponse{AttemptIndex: 0, State: "in_progress"},
		runnerEventResult: RunnerEventResult{Accepted: true},
	}
	handler := newRunMutHandlerNoAuth(store)

	// Missing job_id → 400
	body, _ := json.Marshal(RunnerEventRequest{Seq: 1, Event: "log"})
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/11/runs/2/run/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing job_id: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// seq=0 → 400
	body, _ = json.Marshal(RunnerEventRequest{JobID: "job1", Seq: 0, Event: "log"})
	req = httptest.NewRequest(http.MethodPost, "/v1/projects/proj/issues/11/runs/2/run/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("seq=0: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
