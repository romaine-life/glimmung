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

func TestGitHubActionsTestSlotImageResolverResolvesPRLookupTagAndValidates(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestGitHubActionsTestSlotImageResolverResolvesPRNumberFromCommitWhenRunOmitsIt(t *testing.T) {
	// GitHub's workflow_runs API frequently returns an empty pull_requests array
	// for same-repo PR runs (verified against tank-operator #1295). The resolver
	// must recover the PR number from the head commit and build the ci-pr tag CI
	// actually pushed -- not fall back to ci-ref-<hash>, which is the 2026-06-18
	// deploy-image regression.
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/romaine-life/tank-operator/actions/workflows/docker-build-check.yaml/runs":
			_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":12345,"run_attempt":1,"event":"pull_request","status":"completed","conclusion":"success","head_sha":"abc123def456","pull_requests":[]}]}`)
		case "/repos/romaine-life/tank-operator/commits/abc123def456/pulls":
			_, _ = fmt.Fprint(w, `[{"number":1295,"state":"open"}]`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
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
	want := "romainecr.azurecr.io/tank-operator:ci-pr-1295-run-12345-attempt-1"
	if resolved.Image != want {
		t.Fatalf("image=%q, want %q (must resolve PR number from commit, not ci-ref)", resolved.Image, want)
	}
}

func TestGitHubActionsTestSlotImageResolverFallsBackToDispatchLookupTag(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	_, err := ciLookupTagForWorkflowRun(githubWorkflowRun{Event: "workflow_dispatch"}, "abc123", 0)
	if err == nil || !strings.Contains(err.Error(), "workflow run id is required") {
		t.Fatalf("err=%v, want missing run id", err)
	}
}

func deployImageResolverProject() Project {
	return Project{
		Name: "tank-operator",
		Metadata: map[string]any{
			"test_slot_deploy": map[string]any{
				"ci_image": map[string]any{
					"repository": "romainecr.azurecr.io/tank-operator",
				},
			},
		},
	}
}

// TestGitHubActionsTestSlotImageResolverReturnsPendingWhileBuildRunning pins the
// 2026-06-20 race: a test slot requested while the PR's docker-build-check run is
// still in_progress must yield a typed, retryable pending error — NOT a probe of
// the registry for a fabricated ci-ref tag the PR build never pushes.
func TestGitHubActionsTestSlotImageResolverReturnsPendingWhileBuildRunning(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("head_sha"); got != "racysha" {
			t.Fatalf("head_sha=%q, want racysha", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":42,"run_attempt":1,"event":"pull_request","status":"in_progress","head_sha":"racysha","pull_requests":[{"number":81}]}]}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", "racysha", "gh-token")
	var pending *testSlotCIImagePendingError
	if !errors.As(err, &pending) {
		t.Fatalf("err=%v, want *testSlotCIImagePendingError", err)
	}
	if pending.RunID != 42 || pending.Status != "in_progress" {
		t.Fatalf("pending=%#v, want run 42 in_progress", pending)
	}
	if !strings.Contains(err.Error(), "not ready yet") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("message=%q, want actionable pending text", err.Error())
	}
	if strings.Contains(err.Error(), "not found in registry") {
		t.Fatalf("pending must not surface a registry miss: %q", err.Error())
	}
	if len(validator.seen) != 0 {
		t.Fatalf("must not probe ACR for an in-progress build; saw %#v", validator.seen)
	}
	if requests != 1 {
		t.Fatalf("requests=%d, want a single head_sha lookup (no dispatch fallback)", requests)
	}
}

// TestGitHubActionsTestSlotImageResolverReportsFailedBuild: a completed-but-failed
// docker-build-check run for the commit is terminal and names the run, not a
// retryable pending state and not a registry miss.
func TestGitHubActionsTestSlotImageResolverReportsFailedBuild(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":7,"run_attempt":1,"event":"pull_request","status":"completed","conclusion":"failure","head_sha":"badsha","pull_requests":[{"number":81}]}]}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", "badsha", "gh-token")
	var pending *testSlotCIImagePendingError
	if errors.As(err, &pending) {
		t.Fatalf("failed build must not be reported as pending: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "concluded failure") || !strings.Contains(err.Error(), "no CI image was published") {
		t.Fatalf("err=%v, want terminal failed-build message naming the run", err)
	}
	if len(validator.seen) != 0 {
		t.Fatalf("must not probe ACR for a failed build; saw %#v", validator.seen)
	}
}

// TestGitHubActionsTestSlotImageResolverNoBuildIsClearNotRegistryMiss: when no
// run targets the commit and the dispatch probe finds nothing, the error must be
// a clear "no CI image for commit" — never the fabricated "tag ... not found in
// registry" the old recent-runs fallback surfaced for PR commits.
func TestGitHubActionsTestSlotImageResolverNoBuildIsClearNotRegistryMiss(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workflow_runs":[]}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", "ghostsha", "gh-token")
	if err == nil || !strings.Contains(err.Error(), "no CI image for commit ghostsha") {
		t.Fatalf("err=%v, want clear no-build message", err)
	}
	if strings.Contains(err.Error(), "not found in registry") {
		t.Fatalf("message must not surface a fabricated registry miss: %q", err.Error())
	}
	if len(validator.seen) != 0 {
		t.Fatalf("nothing to validate when no run targets the commit; saw %#v", validator.seen)
	}
}

// TestGitHubActionsTestSlotImageResolverPrefersSuccessfulRerunOverFailedAttempt:
// GitHub returns runs newest-first; a green re-run must win over an earlier
// failed attempt for the same commit.
func TestGitHubActionsTestSlotImageResolverPrefersSuccessfulRerunOverFailedAttempt(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"workflow_runs":[{"id":200,"run_attempt":2,"event":"pull_request","status":"completed","conclusion":"success","head_sha":"mixedsha","pull_requests":[{"number":81}]},{"id":199,"run_attempt":1,"event":"pull_request","status":"completed","conclusion":"failure","head_sha":"mixedsha","pull_requests":[{"number":81}]}]}`)
	}))
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{}
	resolved, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", "mixedsha", "gh-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Tag != "ci-pr-81-run-200-attempt-2" {
		t.Fatalf("tag=%q, want green re-run ci-pr-81-run-200-attempt-2", resolved.Tag)
	}
}
