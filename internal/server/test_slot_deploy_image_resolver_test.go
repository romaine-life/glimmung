package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordingImageValidator struct {
	seen []ResolvedTestSlotImage
	err  error
}

func (v *recordingImageValidator) ValidateTestSlotImage(_ context.Context, image ResolvedTestSlotImage) error {
	v.seen = append(v.seen, image)
	return v.err
}

func serveHappyDeployImageGate(t *testing.T, w http.ResponseWriter, r *http.Request, sha string) bool {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer gh-token" {
		t.Fatalf("authorization=%q", got)
	}
	prefix := "/repos/romaine-life/tank-operator"
	switch r.URL.Path {
	case prefix + "/compare/main..." + sha:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ahead","behind_by":0}`)
		return true
	case prefix + "/commits/" + sha + "/pulls":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{"number":77,"state":"open","head":{"sha":%q},"base":{"ref":"main"}}]`, sha)
		return true
	case prefix + "/pulls/77":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"number":77,"mergeable":true,"mergeable_state":"clean","head":{"sha":%q},"base":{"ref":"main"}}`, sha)
		return true
	case prefix + "/commits/" + sha + "/check-runs":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"check_runs":[{"id":1,"name":"docker-build-check","status":"completed","conclusion":"success","started_at":"2026-06-17T00:00:00Z"}]}`)
		return true
	case prefix + "/commits/" + sha + "/status":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"state":"success","statuses":[{"context":"legacy-ci","state":"success"}]}`)
		return true
	default:
		return false
	}
}

func TestGitHubActionsTestSlotImageResolverResolvesPRLookupTagAndValidates(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveHappyDeployImageGate(t, w, r, "abc123def456") {
			return
		}
		if r.URL.Path != "/repos/romaine-life/tank-operator/actions/workflows/docker-build-check.yaml/runs" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if got := r.URL.Query().Get("head_sha"); got != "abc123def456" {
			t.Fatalf("head_sha=%q, want abc123def456", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gh-token" {
			t.Fatalf("authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":12345,"run_attempt":2,"event":"pull_request","status":"completed","conclusion":"success","pull_requests":[{"number":77}]}]}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
				},
			},
		},
	}
	validator := &recordingImageValidator{}
	resolved, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), project, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantImage := "romainecr.azurecr.io/tank-operator:ci-pr-77-run-12345-attempt-2"
	if resolved.Image != wantImage {
		t.Fatalf("image=%q, want %q", resolved.Image, wantImage)
	}
	if resolved.Tag != "ci-pr-77-run-12345-attempt-2" {
		t.Fatalf("tag=%q", resolved.Tag)
	}
	if resolved.Source != "github_actions:docker-build-check.yaml:run:12345:attempt:2" {
		t.Fatalf("source=%q", resolved.Source)
	}
	if len(validator.seen) != 1 || validator.seen[0].Image != wantImage {
		t.Fatalf("validator saw %#v, want %s", validator.seen, wantImage)
	}
}

func TestGitHubActionsTestSlotImageResolverFallsBackToDispatchLookupTag(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveHappyDeployImageGate(t, w, r, "abc123def456") {
			return
		}
		if r.URL.Path != "/repos/romaine-life/tank-operator/actions/workflows/docker-build-check.yaml/runs" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		requests++
		switch requests {
		case 1:
			if got := r.URL.Query().Get("head_sha"); got != "abc123def456" {
				t.Fatalf("first head_sha=%q, want abc123def456", got)
			}
			_, _ = fmt.Fprint(w, `{"workflow_runs":[]}`)
		case 2:
			if got := r.URL.Query().Get("head_sha"); got != "" {
				t.Fatalf("second head_sha=%q, want empty", got)
			}
			_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":222,"run_attempt":1,"event":"workflow_dispatch","status":"completed","conclusion":"success"}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
				},
			},
		},
	}
	validator := &recordingImageValidator{}
	resolved, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), project, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantTag := "ci-ref-" + shortRefHash("abc123def456") + "-run-222-attempt-1"
	if resolved.Image != "romainecr.azurecr.io/tank-operator:"+wantTag {
		t.Fatalf("image=%q", resolved.Image)
	}
	if resolved.Tag != wantTag {
		t.Fatalf("tag=%q, want %q", resolved.Tag, wantTag)
	}
	if len(validator.seen) != 1 || validator.seen[0].Tag != wantTag {
		t.Fatalf("validator saw %#v, want tag %s", validator.seen, wantTag)
	}
}

func TestGitHubActionsTestSlotImageResolverFailsWhenLookupTagMissing(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveHappyDeployImageGate(t, w, r, "abc123def456") {
			return
		}
		if r.URL.Path != "/repos/romaine-life/tank-operator/actions/workflows/docker-build-check.yaml/runs" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":12345,"run_attempt":2,"event":"pull_request","status":"completed","conclusion":"success","pull_requests":[{"number":77}]}]}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	want := errors.New("tag missing")
	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
				},
			},
		},
	}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), &recordingImageValidator{err: want})(context.Background(), project, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "validate CI lookup image") {
		t.Fatalf("err=%v, want validation context wrapping %v", err, want)
	}
}

func TestGitHubActionsTestSlotImageResolverRejectsBehindMainBeforeCILookup(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/repos/romaine-life/tank-operator/compare/main...abc123def456" {
			t.Fatalf("unexpected path=%s; behind-main refs must fail before workflow lookup", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"behind","behind_by":7}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
				},
			},
		},
	}
	validator := &recordingImageValidator{}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), project, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err == nil || !strings.Contains(err.Error(), "does not contain current main") {
		t.Fatalf("err=%v, want behind-main rejection", err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d, want only compare request", requests)
	}
	if len(validator.seen) != 0 {
		t.Fatalf("validator saw %#v; behind-main refs must not reach image validation", validator.seen)
	}
}

func TestGitHubActionsTestSlotImageResolverRejectsMergeConflictBeforeCILookup(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/repos/romaine-life/tank-operator/compare/main...abc123def456":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"status":"ahead","behind_by":0}`)
		case "/repos/romaine-life/tank-operator/commits/abc123def456/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[{"number":77,"state":"open","head":{"sha":"abc123def456"},"base":{"ref":"main"}}]`)
		case "/repos/romaine-life/tank-operator/pulls/77":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"number":77,"mergeable":false,"mergeable_state":"dirty","head":{"sha":"abc123def456"},"base":{"ref":"main"}}`)
		default:
			t.Fatalf("unexpected path=%s; merge conflicts must fail before CI lookup", r.URL.Path)
		}
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	project := Project{Name: "tank-operator"}
	validator := &recordingImageValidator{}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), project, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err == nil || !strings.Contains(err.Error(), "resolve merge conflicts") {
		t.Fatalf("err=%v, want merge conflict rejection", err)
	}
	if requests != 3 {
		t.Fatalf("requests=%d, want compare + PR list + PR detail", requests)
	}
	if len(validator.seen) != 0 {
		t.Fatalf("validator saw %#v; conflicted PRs must not reach image validation", validator.seen)
	}
}

func TestGitHubActionsTestSlotImageResolverRejectsNoAssociatedPRBeforeCILookup(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/romaine-life/tank-operator/compare/main...abc123def456":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"status":"ahead","behind_by":0}`)
		case "/repos/romaine-life/tank-operator/commits/abc123def456/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `[]`)
		default:
			t.Fatalf("unexpected path=%s; commits without PRs must fail before CI lookup", r.URL.Path)
		}
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	_, err := githubActionsTestSlotImageResolver(srv.Client(), nil)(context.Background(), Project{Name: "tank-operator"}, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err == nil || !strings.Contains(err.Error(), "no associated open pull request") {
		t.Fatalf("err=%v, want no-PR rejection", err)
	}
}

func TestGitHubActionsTestSlotImageResolverRejectsPendingCheckBeforeCILookup(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/romaine-life/tank-operator/commits/abc123def456/check-runs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"check_runs":[{"id":1,"name":"docker-build-check","status":"in_progress"}]}`)
			return
		}
		if r.URL.Path == "/repos/romaine-life/tank-operator/commits/abc123def456/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"state":"success","statuses":[]}`)
			return
		}
		if serveHappyDeployImageGate(t, w, r, "abc123def456") {
			return
		}
		t.Fatalf("unexpected path=%s; pending checks must fail before workflow lookup", r.URL.Path)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	_, err := githubActionsTestSlotImageResolver(srv.Client(), nil)(context.Background(), Project{Name: "tank-operator"}, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err == nil || !strings.Contains(err.Error(), "not fully green") {
		t.Fatalf("err=%v, want pending CI rejection", err)
	}
}

func TestGitHubActionsTestSlotImageResolverRejectsFailingStatusBeforeCILookup(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/romaine-life/tank-operator/commits/abc123def456/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"state":"failure","statuses":[{"context":"lint","state":"failure"}]}`)
			return
		}
		if serveHappyDeployImageGate(t, w, r, "abc123def456") {
			return
		}
		t.Fatalf("unexpected path=%s; failing statuses must fail before workflow lookup", r.URL.Path)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	_, err := githubActionsTestSlotImageResolver(srv.Client(), nil)(context.Background(), Project{Name: "tank-operator"}, "romaine-life/tank-operator", "abc123def456", "gh-token")
	if err == nil || !strings.Contains(err.Error(), "lint: failure") {
		t.Fatalf("err=%v, want failing status rejection", err)
	}
}

func TestTestSlotCIImageConfigParsesFullRepository(t *testing.T) {
	project := Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "example.azurecr.io/team/tank-operator",
				},
			},
		},
	}
	settings := testSlotCIImageConfig(project)
	if settings.Registry != "example.azurecr.io" || settings.Repository != "team/tank-operator" {
		t.Fatalf("settings=%#v, want registry/repository parsed from full repository", settings)
	}
}

func TestCILookupTagForWorkflowRunRejectsMissingRunID(t *testing.T) {
	_, err := ciLookupTagForWorkflowRun(githubWorkflowRun{Event: "workflow_dispatch"}, "abc123")
	if err == nil || !strings.Contains(err.Error(), "workflow run id is required") {
		t.Fatalf("err=%v, want missing run id", err)
	}
}
