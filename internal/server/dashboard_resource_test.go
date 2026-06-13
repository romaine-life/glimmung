package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newDashboardTestHandler(t *testing.T, store ReadStore) http.Handler {
	t.Helper()
	dir := t.TempDir()
	index := `<!doctype html><html><head><title>glimmung</title></head><body><div id="root"></div></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewWithStore(Settings{StaticDir: dir}, store)
}

// A run-and-deeper dashboard URL fetched as JSON returns the RunReport plus the
// focus and typed-read links for the addressed step — and the run store is
// addressed by the canonical dotted run.cycle, never the bare URL segment.
func TestDashboardURLServesRunReportJSON(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeRunStore{rows: []RunReport{{
		Project:     "ambience",
		Workflow:    "default",
		IssueNumber: 168,
		State:       "failed",
		StartedAt:   now,
		UpdatedAt:   now,
	}}}
	handler := newDashboardTestHandler(t, store)

	const stepPath = "/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify/steps/run-verification"
	req := httptest.NewRequest(http.MethodGet, stepPath, nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res dashboardResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if res.Kind != "step" {
		t.Fatalf("kind=%q", res.Kind)
	}
	if res.Report == nil || res.Report.IssueNumber != 168 {
		t.Fatalf("report=%+v", res.Report)
	}
	if res.Focus == nil || res.Focus.Phase != "llm-verify" || res.Focus.Job != "llm-verify" || res.Focus.Step != "run-verification" {
		t.Fatalf("focus=%+v", res.Focus)
	}
	if res.CanonicalURL != stepPath {
		t.Fatalf("canonical=%q", res.CanonicalURL)
	}
	events := res.Links["runner_events"]
	if !strings.Contains(events, "job_id=llm-verify") || !strings.Contains(events, "step_slug=run-verification") {
		t.Fatalf("runner_events link=%q", events)
	}
	if res.Links["run_report"] != "/v1/projects/ambience/issues/168/runs/9.1/report" {
		t.Fatalf("run_report link=%q", res.Links["run_report"])
	}
	if store.runNumber != "9.1" {
		t.Fatalf("store addressed by run number %q, want canonical 9.1", store.runNumber)
	}
}

// An issue dashboard URL fetched as JSON (via ?format=json) returns the issue
// detail.
func TestDashboardURLServesIssueJSON(t *testing.T) {
	number := 168
	store := fakeGraphStore{issue: IssueDetail{
		Project: "ambience",
		Number:  &number,
		Title:   "broken thing",
		State:   "open",
	}}
	handler := newDashboardTestHandler(t, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects/ambience/issues/168?format=json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var res dashboardResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if res.Kind != "issue" || res.Issue == nil || res.Issue.Title != "broken thing" {
		t.Fatalf("res=%+v issue=%+v", res, res.Issue)
	}
}

// Navigation surfaces (project, runs index) are not single fetchable resources.
func TestDashboardURLNavigationSurfacesAre404(t *testing.T) {
	store := &fakeRunStore{rows: []RunReport{{Project: "ambience", IssueNumber: 168}}}
	handler := newDashboardTestHandler(t, store)
	for _, path := range []string{
		"/projects/ambience?format=json",
		"/projects/ambience/issues/168/runs?format=json",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

// A JSON request for a path that is not a recognized dashboard resource — here
// an ambiguous bare run number — returns 404 JSON, never the SPA shell.
func TestDashboardURLBareRunJSONIs404(t *testing.T) {
	handler := newDashboardTestHandler(t, &fakeRunStore{rows: []RunReport{{Project: "ambience", IssueNumber: 168}}})
	req := httptest.NewRequest(http.MethodGet, "/projects/ambience/issues/168/runs/9", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// The HTML/OG path must never enrich an ambiguous bare run number: it does not
// parse, so even with a matching run in the store the SPA is served with no
// og:image. This is the publicids defect guard on the embed surface.
func TestServeSPAWithOGSkipsAmbiguousBareRun(t *testing.T) {
	now := time.Now()
	store := &fakeRunStore{rows: []RunReport{{
		Project:     "glimmung",
		Workflow:    "default",
		IssueNumber: 141,
		State:       "passed",
		StartedAt:   now,
		UpdatedAt:   now,
	}}}
	handler := newDashboardTestHandler(t, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/projects/glimmung/issues/141/runs/9", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "og:image") {
		t.Fatalf("ambiguous bare run must not be enriched: %s", rec.Body.String())
	}
}
