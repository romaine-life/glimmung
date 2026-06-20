package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const acrTokenScope = "https://containerregistry.azure.net/.default"

// ResolvedTestSlotImage is the deploy-image contract after SHA resolution:
// Image is the concrete ref Helm receives, while Registry/Repository/Tag are
// kept separate so the resolver can prove the tag exists before the slot is
// mutated.
type ResolvedTestSlotImage struct {
	Image      string
	Registry   string
	Repository string
	Tag        string
	Source     string
}

type testSlotImageValidator interface {
	ValidateTestSlotImage(ctx context.Context, image ResolvedTestSlotImage) error
}

type noopTestSlotImageValidator struct{}

func (noopTestSlotImageValidator) ValidateTestSlotImage(context.Context, ResolvedTestSlotImage) error {
	return nil
}

type testSlotCIImageSettings struct {
	Registry   string
	Repository string
	Workflow   string
}

type githubWorkflowRun struct {
	ID           int64  `json:"id"`
	RunAttempt   int    `json:"run_attempt"`
	Event        string `json:"event"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

func githubActionsTestSlotImageResolver(httpClient *http.Client, validator testSlotImageValidator) testSlotImageResolver {
	if validator == nil {
		validator = noopTestSlotImageValidator{}
	}
	return func(ctx context.Context, project Project, slug, sha, token string) (ResolvedTestSlotImage, error) {
		resolved, err := resolveTestSlotImageFromGitHubActions(ctx, httpClient, project, slug, sha, token, validator)
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		return resolved, nil
	}
}

func resolveTestSlotImageFromGitHubActions(ctx context.Context, httpClient *http.Client, project Project, slug, sha, token string, validator testSlotImageValidator) (ResolvedTestSlotImage, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("commit sha is required")
	}
	if strings.TrimSpace(slug) == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("github repo slug is required")
	}
	settings := testSlotCIImageConfig(project)

	// PR builds (ci-pr-<n> tag) and push builds (ci-ref-<hash> tag) both run with
	// head_sha == the commit. Fetch them at every status so the resolver can tell
	// a published image from a build that is still running or has already failed,
	// instead of reading "no successful run yet" as "no image will ever exist".
	commitRuns, err := listWorkflowRunsForCommit(ctx, httpClient, slug, settings.Workflow, sha, token)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	if run, ok := firstWorkflowRun(commitRuns, workflowRunSucceeded); ok {
		prNumber, err := prNumberForWorkflowRun(ctx, httpClient, slug, run, token)
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		resolved, err := resolvedTestSlotImageForWorkflowRun(settings, run, sha, prNumber)
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		if err := validator.ValidateTestSlotImage(ctx, resolved); err != nil {
			return ResolvedTestSlotImage{}, fmt.Errorf("validate CI lookup image for workflow run %d attempt %d: %w", run.ID, run.RunAttempt, err)
		}
		return resolved, nil
	}

	// A run targets this commit but has not published an image. Surface the real
	// state: an in-progress build is retryable (the lookup tag lands when the run
	// reaches its "Create CI lookup tag" step), while a failed build is terminal.
	// Either is clearer — and correct — than probing the registry for a tag this
	// commit's build never pushed (the 2026-06-20 deploy-image race).
	if run, ok := firstWorkflowRun(commitRuns, workflowRunInProgress); ok {
		return ResolvedTestSlotImage{}, &testSlotCIImagePendingError{
			SHA:      sha,
			Workflow: settings.Workflow,
			RunID:    run.ID,
			Status:   strings.TrimSpace(run.Status),
		}
	}
	if run, ok := firstWorkflowRun(commitRuns, workflowRunFailed); ok {
		conclusion := strings.TrimSpace(run.Conclusion)
		if conclusion == "" {
			conclusion = "unsuccessfully"
		}
		return ResolvedTestSlotImage{}, fmt.Errorf("%s run %d for commit %s concluded %s; no CI image was published — fix CI and redeploy", settings.Workflow, run.ID, sha, conclusion)
	}

	// No run targets this commit by head_sha. A workflow_dispatch can build an
	// arbitrary git_ref whose head_sha is the dispatch branch rather than the
	// source SHA, so scan recent successful non-PR runs for the run-scoped ci-ref
	// tag derived from THIS sha. Only the run that actually built this sha pushed
	// that exact tag, so a registry miss here means "not this run" and is skipped
	// rather than surfaced as the resolution failure.
	recent, err := listRecentSuccessfulWorkflowRuns(ctx, httpClient, slug, settings.Workflow, token)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	for _, run := range recent {
		if run.Event == "pull_request" {
			continue
		}
		resolved, err := resolvedTestSlotImageForWorkflowRun(settings, run, sha, 0)
		if err != nil {
			continue
		}
		if err := validator.ValidateTestSlotImage(ctx, resolved); err != nil {
			continue
		}
		return resolved, nil
	}
	return ResolvedTestSlotImage{}, fmt.Errorf("no CI image for commit %s in workflow %s: no successful or in-progress build targets this commit, and no workflow_dispatch build published a matching ci-ref tag", sha, settings.Workflow)
}

// testSlotCIImagePendingError signals that a docker-build-check run targets the
// commit but has not published its CI image yet (the run is queued or in
// progress). It is retryable: the lookup tag lands when the run reaches its
// "Create CI lookup tag" step, so deployImageToTestSlot maps it to 409 with an
// actionable message rather than the registry-miss 422 a fabricated dispatch tag
// used to produce. errors.As lets the caller branch on it without string-matching.
type testSlotCIImagePendingError struct {
	SHA      string
	Workflow string
	RunID    int64
	Status   string
}

func (e *testSlotCIImagePendingError) Error() string {
	run := ""
	if e.RunID > 0 {
		run = fmt.Sprintf(" run %d", e.RunID)
	}
	status := strings.TrimSpace(e.Status)
	if status == "" {
		status = "in progress"
	}
	return fmt.Sprintf("CI image for commit %s is not ready yet: %s%s is %s; retry once the build completes", e.SHA, e.Workflow, run, status)
}

// workflowRunSucceeded reports a completed run that published its image. The
// empty-status branch keeps fixtures that set only conclusion working.
func workflowRunSucceeded(run githubWorkflowRun) bool {
	if !strings.EqualFold(run.Conclusion, "success") {
		return false
	}
	return run.Status == "" || strings.EqualFold(run.Status, "completed")
}

// workflowRunInProgress reports a run that targets the commit but has not
// finished, so its CI image is not published yet — the retryable race a test
// slot hits when requested in the seconds between PR open and the PR build's
// lookup-tag step. Any non-terminal status (queued, in_progress, requested,
// waiting, pending) counts.
func workflowRunInProgress(run githubWorkflowRun) bool {
	switch strings.ToLower(strings.TrimSpace(run.Status)) {
	case "", "completed":
		return false
	default:
		return true
	}
}

// workflowRunFailed reports a completed run that did not publish an image
// (failure, cancelled, timed_out, ...). Terminal: re-polling will not help.
func workflowRunFailed(run githubWorkflowRun) bool {
	return strings.EqualFold(run.Status, "completed") && !strings.EqualFold(run.Conclusion, "success")
}

func firstWorkflowRun(runs []githubWorkflowRun, match func(githubWorkflowRun) bool) (githubWorkflowRun, bool) {
	for _, run := range runs {
		if match(run) {
			return run, true
		}
	}
	return githubWorkflowRun{}, false
}

func testSlotCIImageConfig(project Project) testSlotCIImageSettings {
	registry := "romainecr.azurecr.io"
	registryExplicit := false
	repository := strings.TrimSpace(project.Name)
	workflow := "docker-build-check.yaml"
	if deploy, ok := testSlotDeployMetadata(project); ok {
		if ciImage, ok := mapFromMap(deploy, "ci_image"); ok {
			if configured := configString(ciImage, "registry"); configured != "" {
				registry = configured
				registryExplicit = true
			}
			repository = firstNonEmpty(
				configString(ciImage, "repository"),
				configString(ciImage, "image_repository", "imageRepository"),
				repository,
			)
			workflow = firstNonEmpty(configString(ciImage, "workflow", "workflow_file", "workflowFile"), workflow)
		} else if ciImage, ok := mapFromMap(deploy, "ciImage"); ok {
			if configured := configString(ciImage, "registry"); configured != "" {
				registry = configured
				registryExplicit = true
			}
			repository = firstNonEmpty(
				configString(ciImage, "repository"),
				configString(ciImage, "image_repository", "imageRepository"),
				repository,
			)
			workflow = firstNonEmpty(configString(ciImage, "workflow", "workflow_file", "workflowFile"), workflow)
		}
	}
	if parsedRegistry, parsedRepository, ok := splitRegistryRepository(repository); ok && !registryExplicit {
		registry = parsedRegistry
		repository = parsedRepository
	}
	return testSlotCIImageSettings{
		Registry:   registry,
		Repository: repository,
		Workflow:   workflow,
	}
}

func testSlotDeployMetadata(project Project) (map[string]any, bool) {
	for _, key := range []string{"test_slot_deploy", "testSlotDeploy"} {
		if raw, ok := mapFromMap(project.Metadata, key); ok {
			return raw, true
		}
	}
	return nil, false
}

// listWorkflowRunsForCommit returns every docker-build-check run whose head_sha
// is the commit, at any status, so the resolver can distinguish a published
// image (successful run) from a build that is still running (retryable) or one
// that failed (terminal). PR builds and push builds run with head_sha == the
// commit; a workflow_dispatch of an arbitrary git_ref does not, and is recovered
// by the recent-runs probe.
func listWorkflowRunsForCommit(ctx context.Context, httpClient *http.Client, slug, workflow, sha, token string) ([]githubWorkflowRun, error) {
	values := url.Values{}
	values.Set("per_page", "20")
	values.Set("head_sha", sha)
	return fetchWorkflowRuns(ctx, httpClient, slug, workflow, token, values)
}

func listRecentSuccessfulWorkflowRuns(ctx context.Context, httpClient *http.Client, slug, workflow, token string) ([]githubWorkflowRun, error) {
	values := url.Values{}
	values.Set("status", "success")
	values.Set("per_page", "50")
	runs, err := fetchWorkflowRuns(ctx, httpClient, slug, workflow, token, values)
	if err != nil {
		return nil, err
	}
	out := make([]githubWorkflowRun, 0, len(runs))
	for _, run := range runs {
		if workflowRunSucceeded(run) {
			out = append(out, run)
		}
	}
	return out, nil
}

// fetchWorkflowRuns returns the raw workflow_runs page for the query with no
// status filtering — callers classify by status/conclusion themselves.
func fetchWorkflowRuns(ctx context.Context, httpClient *http.Client, slug, workflow, token string, query url.Values) ([]githubWorkflowRun, error) {
	var payload struct {
		WorkflowRuns []githubWorkflowRun `json:"workflow_runs"`
	}
	apiURL := githubAPIBase + "/repos/" + slug + "/actions/workflows/" + url.PathEscape(workflow) + "/runs"
	if encoded := query.Encode(); encoded != "" {
		apiURL += "?" + encoded
	}
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &payload); err != nil {
		return nil, err
	}
	return payload.WorkflowRuns, nil
}

func resolvedTestSlotImageForWorkflowRun(settings testSlotCIImageSettings, run githubWorkflowRun, sha string, prNumber int) (ResolvedTestSlotImage, error) {
	tag, err := ciLookupTagForWorkflowRun(run, sha, prNumber)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	return resolvedTestSlotImageFromRepositoryTag(settings.Registry, settings.Repository, tag, fmt.Sprintf("github_actions:%s:run:%d:attempt:%d", settings.Workflow, run.ID, run.RunAttempt))
}

func ciLookupTagForWorkflowRun(run githubWorkflowRun, sha string, prNumber int) (string, error) {
	if run.ID <= 0 {
		return "", fmt.Errorf("workflow run id is required")
	}
	attempt := run.RunAttempt
	if attempt <= 0 {
		attempt = 1
	}
	// docker-build-check tags pull_request builds ci-pr-<number>-... and only
	// non-PR builds ci-ref-<hash>-.... A pull_request run therefore MUST resolve
	// to a PR number; falling back to the ci-ref hash would look up a tag CI
	// never pushes for PR builds (the 2026-06-18 deploy-image regression, where
	// GitHub returned an empty pull_requests array on the run).
	if run.Event == "pull_request" {
		if prNumber <= 0 {
			return "", fmt.Errorf("pull_request workflow run %d has no resolvable PR number for the ci-pr lookup tag", run.ID)
		}
		return fmt.Sprintf("ci-pr-%d-run-%d-attempt-%d", prNumber, run.ID, attempt), nil
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("source sha is required for non-PR CI lookup tag")
	}
	return fmt.Sprintf("ci-ref-%s-run-%d-attempt-%d", shortRefHash(sha), run.ID, attempt), nil
}

// prNumberForWorkflowRun resolves the PR number for a workflow run. GitHub's
// workflow_runs API frequently returns an empty pull_requests array even for
// same-repo pull_request runs, so when the run omits it we resolve the number
// from the head commit. Without this, a pull_request run falls through to the
// ci-ref-<hash> lookup tag that docker-build-check never pushes for PRs.
func prNumberForWorkflowRun(ctx context.Context, httpClient *http.Client, slug string, run githubWorkflowRun, token string) (int, error) {
	if n := firstWorkflowRunPRNumber(run); n > 0 {
		return n, nil
	}
	if run.Event != "pull_request" {
		return 0, nil
	}
	head := strings.TrimSpace(run.HeadSHA)
	if head == "" {
		return 0, fmt.Errorf("pull_request workflow run %d has no head_sha to resolve a PR number", run.ID)
	}
	return lookupPRNumberForCommit(ctx, httpClient, slug, head, token)
}

func firstWorkflowRunPRNumber(run githubWorkflowRun) int {
	for _, pr := range run.PullRequests {
		if pr.Number > 0 {
			return pr.Number
		}
	}
	return 0
}

func lookupPRNumberForCommit(ctx context.Context, httpClient *http.Client, slug, sha, token string) (int, error) {
	var prs []struct {
		Number int `json:"number"`
	}
	apiURL := githubAPIBase + "/repos/" + slug + "/commits/" + url.PathEscape(sha) + "/pulls"
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &prs); err != nil {
		return 0, fmt.Errorf("resolve PR number for commit %s: %w", sha, err)
	}
	for _, pr := range prs {
		if pr.Number > 0 {
			return pr.Number, nil
		}
	}
	return 0, fmt.Errorf("no pull request found for commit %s", sha)
}

func shortRefHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return fmt.Sprintf("%x", sum)[:12]
}

func resolvedTestSlotImageFromRepositoryTag(registry, repository, tag, source string) (ResolvedTestSlotImage, error) {
	repository = strings.TrimSpace(repository)
	tag = strings.TrimSpace(tag)
	if repository == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("test_slot_deploy.ci_image.repository is required")
	}
	if tag == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image tag is required")
	}
	if strings.Contains(tag, "/") || strings.Contains(tag, ":") {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image tag %q must be a tag, not an image ref", tag)
	}
	registry = strings.TrimSpace(registry)
	if parsedRegistry, parsedRepository, ok := splitRegistryRepository(repository); ok {
		if registry == "" || registry == parsedRegistry {
			registry = parsedRegistry
			repository = parsedRepository
		} else {
			return ResolvedTestSlotImage{}, fmt.Errorf("test_slot_deploy.ci_image.repository registry %q conflicts with registry %q", parsedRegistry, registry)
		}
	}
	if registry == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("test_slot_deploy.ci_image.registry is required")
	}
	image := registry + "/" + strings.TrimPrefix(repository, "/") + ":" + tag
	return ResolvedTestSlotImage{
		Image:      image,
		Registry:   registry,
		Repository: strings.TrimPrefix(repository, "/"),
		Tag:        tag,
		Source:     source,
	}, nil
}

func resolvedTestSlotImageFromRef(image, source string) (ResolvedTestSlotImage, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image is required")
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image %q must include an explicit tag", image)
	}
	registryRepository := image[:colon]
	tag := image[colon+1:]
	registry, repository, ok := splitRegistryRepository(registryRepository)
	if !ok {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image %q must include registry/repository:tag", image)
	}
	if strings.TrimSpace(tag) == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image %q has an empty tag", image)
	}
	return ResolvedTestSlotImage{
		Image:      image,
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Source:     source,
	}, nil
}

func splitRegistryRepository(value string) (registry, repository string, ok bool) {
	value = strings.TrimSpace(value)
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	first := parts[0]
	if first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":") {
		return first, strings.TrimPrefix(parts[1], "/"), strings.TrimSpace(parts[1]) != ""
	}
	return "", "", false
}

type acrImageTagValidator struct {
	credential azcore.TokenCredential
	httpClient *http.Client
	tenantID   string
	endpoint   string
}

func newACRImageTagValidator() testSlotImageValidator {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return errorImageTagValidator{err: fmt.Errorf("create Azure credential: %w", err)}
	}
	return &acrImageTagValidator{
		credential: cred,
		httpClient: http.DefaultClient,
		tenantID:   strings.TrimSpace(os.Getenv("AZURE_TENANT_ID")),
	}
}

type errorImageTagValidator struct{ err error }

func (v errorImageTagValidator) ValidateTestSlotImage(context.Context, ResolvedTestSlotImage) error {
	return v.err
}

func (v *acrImageTagValidator) ValidateTestSlotImage(ctx context.Context, image ResolvedTestSlotImage) error {
	if image.Registry == "" || image.Repository == "" || image.Tag == "" {
		return fmt.Errorf("resolved image is missing registry, repository, or tag")
	}
	if !strings.HasSuffix(image.Registry, ".azurecr.io") {
		return fmt.Errorf("registry %q is unsupported for deploy-image validation", image.Registry)
	}
	token, err := v.acrAccessToken(ctx, image.Registry, "repository:"+image.Repository+":pull")
	if err != nil {
		return fmt.Errorf("validate image %s: %w", image.Image, err)
	}
	endpoint := strings.TrimRight(firstNonEmpty(v.endpoint, "https://"+image.Registry), "/")
	manifestURL := endpoint + "/v2/" + strings.TrimPrefix(image.Repository, "/") + "/manifests/" + url.PathEscape(image.Tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("check registry manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("image tag %s not found in registry", image.Image)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("registry manifest check for %s returned %d: %s", image.Image, resp.StatusCode, strings.TrimSpace(string(data)))
}

func (v *acrImageTagValidator) acrAccessToken(ctx context.Context, registry, scope string) (string, error) {
	aad, err := v.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{acrTokenScope}})
	if err != nil {
		return "", fmt.Errorf("get Azure ACR token: %w", err)
	}
	endpoint := strings.TrimRight(firstNonEmpty(v.endpoint, "https://"+registry), "/")
	exchange := url.Values{}
	exchange.Set("grant_type", "access_token")
	exchange.Set("service", registry)
	exchange.Set("access_token", aad.Token)
	if strings.TrimSpace(v.tenantID) != "" {
		exchange.Set("tenant", strings.TrimSpace(v.tenantID))
	}
	var exchanged struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := v.postRegistryForm(ctx, endpoint+"/oauth2/exchange", exchange, &exchanged); err != nil {
		return "", err
	}
	if strings.TrimSpace(exchanged.RefreshToken) == "" {
		return "", fmt.Errorf("ACR token exchange returned no refresh token")
	}

	tokenReq := url.Values{}
	tokenReq.Set("grant_type", "refresh_token")
	tokenReq.Set("service", registry)
	tokenReq.Set("scope", scope)
	tokenReq.Set("refresh_token", exchanged.RefreshToken)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := v.postRegistryForm(ctx, endpoint+"/oauth2/token", tokenReq, &token); err != nil {
		return "", err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return "", fmt.Errorf("ACR scope token response was empty")
	}
	return token.AccessToken, nil
}

func (v *acrImageTagValidator) postRegistryForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return nil
}
