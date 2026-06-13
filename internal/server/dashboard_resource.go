// Dashboard deep links as fetchable resources.
//
// A glimmung dashboard URL such as
//
//	/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify/steps/run-verification
//
// is served as the SPA in a browser, but returns the canonical resource JSON
// when the caller asks for it (Accept: application/json or ?format=json). That
// lets an agent — or a plain `curl` — that was handed a dashboard URL fetch the
// resource straight back instead of re-deriving glimmung's internal addressing
// from the URL. The path is resolved server-side by the single canonical parser
// in internal/domain/publicids (the inverse of the frontend route builder), so
// the bare-run-number defect can never resolve to the wrong run.
//
// Read posture matches the rest of the run read surface (see og_run.go): these
// reads are public, so no token is required.

package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/publicids"
)

// wantsResourceJSON reports whether the caller explicitly asked for the JSON
// resource rather than the dashboard SPA. The signal must be explicit —
// `?format=json`/`format=api`, or an `Accept` header that names
// `application/json` — so link unfurlers and browsers, which send `*/*` or
// `text/html`, keep getting the OG-enriched HTML shell.
func wantsResourceJSON(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format"))) {
	case "json", "api":
		return true
	case "html":
		return false
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

// dashboardResource is the JSON envelope returned when a dashboard deep link is
// fetched as JSON. It resolves the human URL to the canonical Glimmung resource
// — the RunReport (the canonical review object) for run-and-deeper links, or
// the issue detail for an issue link — and points at the typed `/v1` reads for
// whatever the URL addressed.
type dashboardResource struct {
	Kind         string            `json:"kind"`
	CanonicalURL string            `json:"canonical_url"`
	Focus        *dashboardFocus   `json:"focus,omitempty"`
	Links        map[string]string `json:"links"`
	Report       *RunReport        `json:"report,omitempty"`
	Issue        *IssueDetail      `json:"issue,omitempty"`
}

// dashboardFocus names the phase/job/step a deep link pointed at inside the
// RunReport, so an agent does not have to re-derive it from the URL.
type dashboardFocus struct {
	Phase string `json:"phase,omitempty"`
	Job   string `json:"job,omitempty"`
	Step  string `json:"step,omitempty"`
}

// addrHasRunCycle reports whether the address names a specific run cycle (run
// or deeper), i.e. whether a RunReport is the resource to serve.
func addrHasRunCycle(addr publicids.IssueRunAddress) bool {
	switch addr.Kind {
	case publicids.EntityRun, publicids.EntityPhase, publicids.EntityJob, publicids.EntityStep:
		return true
	}
	return false
}

// serveDashboardResourceJSON resolves a parsed dashboard address to its
// canonical resource JSON.
func serveDashboardResourceJSON(w http.ResponseWriter, r *http.Request, store ReadStore, addr publicids.IssueRunAddress) {
	canonical, err := publicids.DashboardPath(addr)
	if err != nil {
		writeInternalError(w, r, err, "render canonical dashboard url failed")
		return
	}
	switch {
	case addrHasRunCycle(addr):
		resolveRunResource(w, r, store, addr, canonical)
	case addr.Kind == publicids.EntityIssue:
		resolveIssueResource(w, r, store, addr, canonical)
	default:
		// project / runs index — navigation surfaces, not single resources.
		writeProblem(w, http.StatusNotFound, fmt.Sprintf(
			"%s is a navigation surface, not a single fetchable resource; fetch a specific issue or run",
			addr.Kind))
	}
}

func resolveRunResource(w http.ResponseWriter, r *http.Request, store ReadStore, addr publicids.IssueRunAddress, canonical string) {
	runStore, ok := store.(RunStore)
	if !ok || runStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
		return
	}
	runDisplay := addr.RunCycle.String()
	report, err := runStore.GetRunReportByNumber(r.Context(), addr.Project, addr.IssueNumber, runDisplay)
	switch {
	case errors.Is(err, ErrNotFound):
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	case err != nil:
		writeInternalError(w, r, err, "get run report failed")
		return
	}
	res := dashboardResource{
		Kind:         string(addr.Kind),
		CanonicalURL: canonical,
		Links:        runResourceLinks(addr, runDisplay),
		Report:       &report,
	}
	if addr.Phase != "" || addr.Job != "" || addr.Step != "" {
		res.Focus = &dashboardFocus{Phase: addr.Phase, Job: addr.Job, Step: addr.Step}
	}
	writeJSON(w, http.StatusOK, res)
}

func resolveIssueResource(w http.ResponseWriter, r *http.Request, store ReadStore, addr publicids.IssueRunAddress, canonical string) {
	issueStore, ok := store.(IssueStore)
	if !ok || issueStore == nil {
		writeProblem(w, http.StatusServiceUnavailable, "issue store not configured")
		return
	}
	detail, err := issueStore.GetIssueDetailByNumber(r.Context(), addr.Project, addr.IssueNumber)
	switch {
	case errors.Is(err, ErrNotFound):
		writeProblem(w, http.StatusNotFound, "issue not found")
		return
	case err != nil:
		writeInternalError(w, r, err, "get issue detail failed")
		return
	}
	writeJSON(w, http.StatusOK, dashboardResource{
		Kind:         string(addr.Kind),
		CanonicalURL: canonical,
		Links: map[string]string{
			"issue": fmt.Sprintf("/v1/issues/by-number/%s/%d", url.PathEscape(addr.Project), addr.IssueNumber),
		},
		Issue: &detail,
	})
}

// runResourceLinks points at the typed /v1 reads for a run cycle, narrowing the
// runner event stream to the addressed job/step when the URL named them. The
// run is addressed by its canonical dotted run.cycle number, the only form the
// /v1 run routes accept.
func runResourceLinks(addr publicids.IssueRunAddress, runDisplay string) map[string]string {
	base := fmt.Sprintf("/v1/projects/%s/issues/%d/runs/%s",
		url.PathEscape(addr.Project), addr.IssueNumber, url.PathEscape(runDisplay))
	events := base + "/run/events"
	q := url.Values{}
	if addr.Job != "" {
		q.Set("job_id", addr.Job)
	}
	if addr.Step != "" {
		q.Set("step_slug", addr.Step)
	}
	if len(q) > 0 {
		events = events + "?" + q.Encode()
	}
	return map[string]string{
		"run_report":    base + "/report",
		"runner_events": events,
		"runner_status": base + "/run/status",
	}
}
