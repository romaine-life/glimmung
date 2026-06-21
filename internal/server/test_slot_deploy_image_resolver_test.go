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

const resolverSHA = "abc123def4567890abc123def4567890abc12345"

// githubRunsServer serves a docker-build-check workflow_runs page for the head_sha
// query the miss-diagnostic issues, and fails any other request.
func githubRunsServer(t *testing.T, sha, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/actions/workflows/docker-build-check.yaml/runs") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("head_sha"); got != sha {
			t.Fatalf("head_sha=%q, want %q", got, sha)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
}

// fatalGitHubServer fails the test if any GitHub call is made — used to prove the
// resolve happy path and non-not-found errors never touch GitHub.
func fatalGitHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected GitHub call: %s", r.URL.Path)
	}))
}

// TestResolvesCommitShaImage pins the core design: resolution is a direct ACR
// lookup of the sha-<commit> alias, with NO GitHub Actions call.
func TestResolvesCommitShaImage(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := fatalGitHubServer(t)
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{}
	resolved, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", resolverSHA, "gh-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantTag := "sha-" + resolverSHA
	wantImage := "romainecr.azurecr.io/tank-operator:" + wantTag
	if resolved.Tag != wantTag || resolved.Image != wantImage {
		t.Fatalf("resolved=%+v, want tag %q image %q", resolved, wantTag, wantImage)
	}
	if resolved.Source != "github_actions:docker-build-check.yaml:commit:"+resolverSHA {
		t.Fatalf("source=%q", resolved.Source)
	}
	if len(validator.seen) != 1 || validator.seen[0].Image != wantImage {
		t.Fatalf("validator saw %#v, want %s", validator.seen, wantImage)
	}
}

// TestResolvesCommitShaImageLowercasesSHA: the alias is case-normalized so a
// mixed-case verified SHA resolves the tag CI actually pushed.
func TestResolvesCommitShaImageLowercasesSHA(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := fatalGitHubServer(t)
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{}
	resolved, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", strings.ToUpper(resolverSHA), "gh-token")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Tag != "sha-"+resolverSHA {
		t.Fatalf("tag=%q, want lowercased sha-%s", resolved.Tag, resolverSHA)
	}
}

// TestResolveInProgressBuildWhenAliasAbsent pins the window: the alias is absent
// while the PR build is still in_progress, so resolution yields a terminal,
// diagnostic error naming the in-progress run — not a bare registry miss, and no
// longer a typed retryable "pending" signal. The provisioning gate now waits on
// the durable ci_image_available row (written by the ACR push webhook), so
// glimmung does not special-case an in-flight build into a retryable 409.
func TestResolveInProgressBuildWhenAliasAbsent(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := githubRunsServer(t, resolverSHA, `{"workflow_runs":[{"id":42,"event":"pull_request","status":"in_progress","head_sha":"`+resolverSHA+`","pull_requests":[{"number":81}]}]}`)
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{err: errTestSlotCIImageNotFound}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", resolverSHA, "gh-token")
	if err == nil {
		t.Fatal("want a diagnostic error for an in-progress build with no alias")
	}
	if !strings.Contains(err.Error(), "run 42") || !strings.Contains(err.Error(), "in_progress") || !strings.Contains(err.Error(), "image published yet") {
		t.Fatalf("message=%q, want in-progress diagnostic naming the run", err.Error())
	}
	if strings.Contains(err.Error(), "not found in registry") {
		t.Fatalf("diagnostic must not surface the raw registry miss: %q", err.Error())
	}
	if len(validator.seen) != 1 || validator.seen[0].Tag != "sha-"+resolverSHA {
		t.Fatalf("validator saw %#v, want one sha- lookup before diagnosing", validator.seen)
	}
}

// TestResolveFailedBuildWhenAliasAbsent: a completed-but-failed build is terminal
// and names the run, not a retryable pending state.
func TestResolveFailedBuildWhenAliasAbsent(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := githubRunsServer(t, resolverSHA, `{"workflow_runs":[{"id":7,"event":"pull_request","status":"completed","conclusion":"failure","head_sha":"`+resolverSHA+`","pull_requests":[{"number":81}]}]}`)
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{err: errTestSlotCIImageNotFound}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", resolverSHA, "gh-token")
	if err == nil || !strings.Contains(err.Error(), "concluded failure") || !strings.Contains(err.Error(), "no CI image was published") {
		t.Fatalf("err=%v, want terminal failed-build message naming the run", err)
	}
}

// TestResolveSucceededButNoAliasWhenAbsent covers the transition window: a build
// that ran before the sha-alias step succeeded but published no alias, so the
// diagnostic tells the operator to re-run rather than emitting a bare 404.
func TestResolveSucceededButNoAliasWhenAbsent(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := githubRunsServer(t, resolverSHA, `{"workflow_runs":[{"id":99,"event":"pull_request","status":"completed","conclusion":"success","head_sha":"`+resolverSHA+`","pull_requests":[{"number":81}]}]}`)
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{err: errTestSlotCIImageNotFound}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", resolverSHA, "gh-token")
	if err == nil || !strings.Contains(err.Error(), "succeeded but published no") || !strings.Contains(err.Error(), "re-run the build") {
		t.Fatalf("err=%v, want succeeded-but-no-alias guidance", err)
	}
}

// TestResolveNoBuildWhenAliasAbsent: nothing targets the commit, so the error is
// a clear "no CI image for commit" — never the raw registry miss.
func TestResolveNoBuildWhenAliasAbsent(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := githubRunsServer(t, resolverSHA, `{"workflow_runs":[]}`)
	defer srv.Close()
	githubAPIBase = srv.URL

	validator := &recordingImageValidator{err: errTestSlotCIImageNotFound}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", resolverSHA, "gh-token")
	if err == nil || !strings.Contains(err.Error(), "no CI image for commit "+resolverSHA) || !strings.Contains(err.Error(), "no build targets this commit") {
		t.Fatalf("err=%v, want clear no-build message", err)
	}
	if strings.Contains(err.Error(), "not found in registry") {
		t.Fatalf("message must not surface the raw registry miss: %q", err.Error())
	}
}

// TestResolveSurfacesNonNotFoundValidationError: a registry/auth error that is
// NOT a 404 is surfaced as-is (wrapped), without a GitHub diagnostic.
func TestResolveSurfacesNonNotFoundValidationError(t *testing.T) {
	restore := githubAPIBase
	defer func() { githubAPIBase = restore }()
	srv := fatalGitHubServer(t)
	defer srv.Close()
	githubAPIBase = srv.URL

	boom := errors.New("registry manifest check returned 503")
	validator := &recordingImageValidator{err: boom}
	_, err := githubActionsTestSlotImageResolver(srv.Client(), validator)(context.Background(), deployImageResolverProject(), "romaine-life/tank-operator", resolverSHA, "gh-token")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "validate CI image") {
		t.Fatalf("err=%v, want validation context wrapping %v", err, boom)
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
