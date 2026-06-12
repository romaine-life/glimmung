package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/auth"
)

type fakeWorkflowControlPinStore struct {
	fakeReadStore
	workflow Workflow
	err      error

	project string
	name    string
	target  string
	pinReq  WorkflowControlPinRequest
	actor   string
	limit   int
	events  []WorkflowControlEvent
}

func (s *fakeWorkflowControlPinStore) PinWorkflowControl(_ context.Context, project, name, target string, req WorkflowControlPinRequest) (Workflow, error) {
	s.project, s.name, s.target, s.pinReq = project, name, target, req
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowControlPinStore) UnpinWorkflowControl(_ context.Context, project, name, target, actor string) (Workflow, error) {
	s.project, s.name, s.target, s.actor = project, name, target, actor
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowControlPinStore) ListWorkflowControlEvents(_ context.Context, project, name string, limit int) ([]WorkflowControlEvent, error) {
	s.project, s.name, s.limit = project, name, limit
	if s.err != nil {
		return nil, s.err
	}
	return s.events, nil
}

func TestParseControlPinTargetGrammar(t *testing.T) {
	cases := []struct {
		target string
		phase  string
		ok     bool
	}{
		{target: "budget", ok: true},
		{target: "pr.recycle_policy", ok: true},
		{target: "phases.llm-verify.recycle_policy", phase: "llm-verify", ok: true},
		{target: "phases..recycle_policy", ok: false},
		{target: "phases.llm-verify.timeout", ok: false},
		{target: "metadata", ok: false},
		{target: "", ok: false},
	}
	for _, tc := range cases {
		phase, err := ParseControlPinTarget(tc.target)
		if tc.ok && err != nil {
			t.Fatalf("target %q: unexpected err %v", tc.target, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("target %q: expected rejection", tc.target)
		}
		if phase != tc.phase {
			t.Fatalf("target %q: phase=%q, want %q", tc.target, phase, tc.phase)
		}
	}
}

func TestControlPinTargetForRecyclePatch(t *testing.T) {
	if got := ControlPinTargetForRecyclePatch(RecyclePatchTargetPR); got != ControlPinTargetPR {
		t.Fatalf("pr target=%q", got)
	}
	if got := ControlPinTargetForRecyclePatch("llm-verify"); got != "phases.llm-verify.recycle_policy" {
		t.Fatalf("phase target=%q", got)
	}
}

func TestValidateWorkflowRegisterRejectsVerificationPhaseWithoutRecyclePolicy(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	for i := range req.Phases {
		if req.Phases[i].Verify {
			req.Phases[i].RecyclePolicy = nil
		}
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must declare recycle_policy explicitly") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want explicit recycle_policy rejection", err)
	}
	if !strings.Contains(err.Error(), `"verify"`) {
		t.Fatalf("rejection must name the phase, got: %v", err)
	}
}

func TestRequestActorComposition(t *testing.T) {
	withUser := func(user auth.User, header string) string {
		req := httptest.NewRequest(http.MethodPost, "/v1/workflows", nil)
		if header != "" {
			req.Header.Set(ActorHeader, header)
		}
		if user != (auth.User{}) {
			req = req.WithContext(context.WithValue(req.Context(), adminUserContextKey{}, user))
		}
		return requestActor(req)
	}

	if got := withUser(auth.User{Sub: "svc:mcp-glimmung", Role: auth.RomaineRoleService}, ""); got != "svc:mcp-glimmung" {
		t.Fatalf("principal only: %q", got)
	}
	if got := withUser(auth.User{Sub: "svc:mcp-glimmung"}, "tank-session:815"); got != "svc:mcp-glimmung via tank-session:815" {
		t.Fatalf("principal+header: %q", got)
	}
	if got := withUser(auth.User{Sub: "svc:bot", ActorEmail: "human@romaine.life"}, ""); got != "svc:bot for human@romaine.life" {
		t.Fatalf("actor email: %q", got)
	}
	if got := withUser(auth.User{}, "tank-session:815"); got != "(unverified) tank-session:815" {
		t.Fatalf("header only must be labeled unverified: %q", got)
	}
	if got := withUser(auth.User{}, ""); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestPinWorkflowControlRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowControlPinStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workflows/ambience/default/control-pins/budget", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestPinWorkflowControlPinsTargetWithActorAndReason(t *testing.T) {
	store := &fakeWorkflowControlPinStore{workflow: Workflow{Project: "ambience", Name: "default"}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "operator@romaine.life"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/v1/workflows/ambience/default/control-pins/phases.llm-verify.recycle_policy",
		strings.NewReader(`{"reason":"verify gate runs without recycling by operator decision"}`),
	)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.target != "phases.llm-verify.recycle_policy" {
		t.Fatalf("target=%q", store.target)
	}
	if store.pinReq.Actor != "operator@romaine.life" {
		t.Fatalf("actor=%q", store.pinReq.Actor)
	}
	if !strings.Contains(store.pinReq.Reason, "operator decision") {
		t.Fatalf("reason=%q", store.pinReq.Reason)
	}
}

func TestPinWorkflowControlRejectsUnpinnableTarget(t *testing.T) {
	store := &fakeWorkflowControlPinStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/workflows/ambience/default/control-pins/metadata", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.target != "" {
		t.Fatalf("store reached with invalid target %q", store.target)
	}
}

func TestUnpinWorkflowControlMapsMissingTo404(t *testing.T) {
	store := &fakeWorkflowControlPinStore{err: ErrNotFound}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "operator@romaine.life"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/default/control-pins/budget", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.actor != "operator@romaine.life" {
		t.Fatalf("actor=%q", store.actor)
	}
}

func TestListWorkflowControlEventsValidatesLimitAndIsPublic(t *testing.T) {
	store := &fakeWorkflowControlPinStore{events: []WorkflowControlEvent{{ID: 7, Action: "pin", Actor: "operator@romaine.life"}}}
	// nil admin authenticator: the events read is public, like other reads.
	handler := NewWithDependencies(Settings{}, store, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workflows/ambience/default/control-events?limit=3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.limit != 3 {
		t.Fatalf("limit=%d", store.limit)
	}
	if !strings.Contains(rec.Body.String(), `"action":"pin"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workflows/ambience/default/control-events?limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status=%d", rec.Code)
	}
}
