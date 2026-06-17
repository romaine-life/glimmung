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

type githubAssociatedPullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type githubPullRequestDetail struct {
	Number         int    `json:"number"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type githubCheckRun struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	App         struct {
		Slug string `json:"slug"`
	} `json:"app"`
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
	if err := requireDeployImageGitHubGate(ctx, httpClient, slug, sha, token); err != nil {
		return ResolvedTestSlotImage{}, err
	}
	settings := testSlotCIImageConfig(project)
	runs, err := listSuccessfulWorkflowRuns(ctx, httpClient, slug, settings.Workflow, sha, token)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	if len(runs) > 0 {
		run := runs[0]
		resolved, err := resolvedTestSlotImageForWorkflowRun(settings, run, sha)
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		if err := validator.ValidateTestSlotImage(ctx, resolved); err != nil {
			return ResolvedTestSlotImage{}, fmt.Errorf("validate CI lookup image for workflow run %d attempt %d: %w", run.ID, run.RunAttempt, err)
		}
		return resolved, nil
	}

	// workflow_dispatch can build an arbitrary input ref after the run has
	// started, so GitHub's workflow_run.head_sha may be the dispatch branch
	// rather than the resolved image source SHA. In that case scan recent
	// successful non-PR runs and find the run-scoped lookup tag whose ref hash
	// was derived from the resolved source SHA.
	recent, err := listRecentSuccessfulWorkflowRuns(ctx, httpClient, slug, settings.Workflow, token)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	var lastValidationErr error
	for _, run := range recent {
		if run.Event == "pull_request" {
			continue
		}
		resolved, err := resolvedTestSlotImageForWorkflowRun(settings, run, sha)
		if err != nil {
			continue
		}
		if err := validator.ValidateTestSlotImage(ctx, resolved); err != nil {
			lastValidationErr = err
			continue
		}
		return resolved, nil
	}
	if lastValidationErr != nil {
		return ResolvedTestSlotImage{}, fmt.Errorf("no validated CI lookup image for commit %s in workflow %s: %w", sha, settings.Workflow, lastValidationErr)
	}
	return ResolvedTestSlotImage{}, fmt.Errorf("no successful app-image workflow run with a CI lookup tag for commit %s in workflow %s", sha, settings.Workflow)
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

func listSuccessfulWorkflowRuns(ctx context.Context, httpClient *http.Client, slug, workflow, sha, token string) ([]githubWorkflowRun, error) {
	values := url.Values{}
	values.Set("status", "success")
	values.Set("per_page", "20")
	values.Set("head_sha", sha)
	return listWorkflowRuns(ctx, httpClient, slug, workflow, token, values)
}

func listRecentSuccessfulWorkflowRuns(ctx context.Context, httpClient *http.Client, slug, workflow, token string) ([]githubWorkflowRun, error) {
	values := url.Values{}
	values.Set("status", "success")
	values.Set("per_page", "50")
	return listWorkflowRuns(ctx, httpClient, slug, workflow, token, values)
}

func requireCommitContainsMain(ctx context.Context, httpClient *http.Client, slug, sha, token string) error {
	var payload struct {
		Status   string `json:"status"`
		BehindBy int    `json:"behind_by"`
	}
	apiURL := githubAPIBase + "/repos/" + slug + "/compare/" + url.PathEscape("main") + "..." + url.PathEscape(sha)
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &payload); err != nil {
		return fmt.Errorf("verify commit contains main: %w", err)
	}
	status := strings.TrimSpace(strings.ToLower(payload.Status))
	if status == "behind" || status == "diverged" || payload.BehindBy > 0 {
		return fmt.Errorf("commit %s does not contain current main (compare status=%s behind_by=%d); merge main before deploying to a test slot", sha, firstNonEmpty(status, "unknown"), payload.BehindBy)
	}
	return nil
}

func requireDeployImageGitHubGate(ctx context.Context, httpClient *http.Client, slug, sha, token string) error {
	if err := requireCommitContainsMain(ctx, httpClient, slug, sha, token); err != nil {
		return err
	}
	if _, err := requireAssociatedMergeablePullRequest(ctx, httpClient, slug, sha, token); err != nil {
		return err
	}
	if err := requireCommitCIGreen(ctx, httpClient, slug, sha, token); err != nil {
		return err
	}
	return nil
}

func requireAssociatedMergeablePullRequest(ctx context.Context, httpClient *http.Client, slug, sha, token string) (int, error) {
	var pulls []githubAssociatedPullRequest
	apiURL := githubAPIBase + "/repos/" + slug + "/commits/" + url.PathEscape(sha) + "/pulls"
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &pulls); err != nil {
		return 0, fmt.Errorf("verify commit pull request: %w", err)
	}
	for _, pr := range pulls {
		if strings.ToLower(strings.TrimSpace(pr.State)) != "open" {
			continue
		}
		if headSHA := strings.TrimSpace(pr.Head.SHA); headSHA != "" && headSHA != sha {
			continue
		}
		if baseRef := strings.TrimSpace(pr.Base.Ref); baseRef != "" && baseRef != "main" {
			continue
		}
		if pr.Number <= 0 {
			continue
		}
		return pr.Number, requirePullRequestMergeable(ctx, httpClient, slug, pr.Number, sha, token)
	}
	return 0, fmt.Errorf("commit %s has no associated open pull request targeting main at that head; open a PR before deploying to a test slot", sha)
}

func requirePullRequestMergeable(ctx context.Context, httpClient *http.Client, slug string, number int, sha, token string) error {
	var detail githubPullRequestDetail
	apiURL := githubAPIBase + "/repos/" + slug + "/pulls/" + url.PathEscape(fmt.Sprint(number))
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &detail); err != nil {
		return fmt.Errorf("verify pull request mergeability: %w", err)
	}
	if headSHA := strings.TrimSpace(detail.Head.SHA); headSHA != "" && headSHA != sha {
		return fmt.Errorf("PR #%d head is %s, not %s; deploy the current PR head", number, headSHA, sha)
	}
	if baseRef := strings.TrimSpace(detail.Base.Ref); baseRef != "" && baseRef != "main" {
		return fmt.Errorf("PR #%d targets %s, not main; retarget before deploying to a test slot", number, baseRef)
	}
	state := strings.ToLower(strings.TrimSpace(detail.MergeableState))
	if detail.Mergeable == nil || state == "unknown" {
		return fmt.Errorf("PR #%d mergeability is still unknown; retry after GitHub computes mergeability", number)
	}
	if !*detail.Mergeable || state == "dirty" {
		return fmt.Errorf("PR #%d is not mergeable (mergeable_state=%s); resolve merge conflicts before deploying to a test slot", number, firstNonEmpty(state, "mergeable=false"))
	}
	return nil
}

func requireCommitCIGreen(ctx context.Context, httpClient *http.Client, slug, sha, token string) error {
	checkRuns, err := listCommitCheckRuns(ctx, httpClient, slug, sha, token)
	if err != nil {
		return fmt.Errorf("verify commit check-runs: %w", err)
	}
	combinedStatus, err := getCommitCombinedStatus(ctx, httpClient, slug, sha, token)
	if err != nil {
		return fmt.Errorf("verify commit statuses: %w", err)
	}
	ciStatus, ciReason := commitCIState(checkRuns, combinedStatus)
	if ciStatus != "succeeded" {
		return fmt.Errorf("CI for commit %s is not fully green: %s", sha, ciReason)
	}
	return nil
}

func listCommitCheckRuns(ctx context.Context, httpClient *http.Client, slug, sha, token string) ([]githubCheckRun, error) {
	var payload struct {
		CheckRuns []githubCheckRun `json:"check_runs"`
	}
	apiURL := githubAPIBase + "/repos/" + slug + "/commits/" + url.PathEscape(sha) + "/check-runs?per_page=100"
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &payload); err != nil {
		return nil, err
	}
	return payload.CheckRuns, nil
}

type githubCombinedStatus struct {
	State    string `json:"state"`
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

func getCommitCombinedStatus(ctx context.Context, httpClient *http.Client, slug, sha, token string) (githubCombinedStatus, error) {
	var payload githubCombinedStatus
	apiURL := githubAPIBase + "/repos/" + slug + "/commits/" + url.PathEscape(sha) + "/status"
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &payload); err != nil {
		return githubCombinedStatus{}, err
	}
	return payload, nil
}

func commitCIState(checkRuns []githubCheckRun, combinedStatus githubCombinedStatus) (string, string) {
	conclusionsOK := map[string]bool{"success": true, "skipped": true, "neutral": true}
	var pending []string
	var failed []string
	completed := 0
	for _, run := range latestCommitCheckRuns(checkRuns) {
		name := firstNonEmpty(strings.TrimSpace(run.Name), strings.TrimSpace(run.App.Slug), "check")
		status := strings.ToLower(strings.TrimSpace(run.Status))
		conclusion := strings.ToLower(strings.TrimSpace(run.Conclusion))
		if status != "completed" {
			pending = append(pending, name)
			continue
		}
		completed++
		if !conclusionsOK[conclusion] {
			failed = append(failed, fmt.Sprintf("%s: %s", name, firstNonEmpty(conclusion, "failed")))
		}
	}
	statuses := 0
	state := strings.ToLower(strings.TrimSpace(combinedStatus.State))
	for _, status := range combinedStatus.Statuses {
		statuses++
		statusState := strings.ToLower(strings.TrimSpace(status.State))
		if statusState == "failure" || statusState == "error" {
			failed = append(failed, fmt.Sprintf("%s: %s", firstNonEmpty(strings.TrimSpace(status.Context), "status"), statusState))
		}
	}
	if (state == "failure" || state == "error") && statuses == 0 {
		failed = append(failed, "combined status: "+state)
	}
	if state == "pending" && statuses > 0 {
		pending = append(pending, "combined status")
	}
	if len(failed) > 0 {
		return "failed", strings.Join(failed, "; ")
	}
	if len(pending) > 0 || (completed == 0 && statuses == 0) {
		return "started", "checks are pending or have not appeared yet"
	}
	return "succeeded", "all observed checks passed"
}

func latestCommitCheckRuns(checkRuns []githubCheckRun) []githubCheckRun {
	latest := map[string]githubCheckRun{}
	for _, run := range checkRuns {
		name := firstNonEmpty(strings.TrimSpace(run.Name), strings.TrimSpace(run.App.Slug), "check")
		if existing, ok := latest[name]; !ok || checkRunRecency(run) >= checkRunRecency(existing) {
			latest[name] = run
		}
	}
	out := make([]githubCheckRun, 0, len(latest))
	for _, run := range latest {
		out = append(out, run)
	}
	return out
}

func checkRunRecency(run githubCheckRun) string {
	return firstNonEmpty(strings.TrimSpace(run.CompletedAt), strings.TrimSpace(run.StartedAt), fmt.Sprintf("%020d", run.ID))
}

func listWorkflowRuns(ctx context.Context, httpClient *http.Client, slug, workflow, token string, query url.Values) ([]githubWorkflowRun, error) {
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
	var out []githubWorkflowRun
	for _, run := range payload.WorkflowRuns {
		if run.Status == "completed" && run.Conclusion == "success" {
			out = append(out, run)
			continue
		}
		// GitHub's status=success filter already limits conclusions on the
		// REST side; keep this tolerant for test fixtures and API drift.
		if run.Conclusion == "" && run.Status == "" {
			out = append(out, run)
		}
	}
	return out, nil
}

func resolvedTestSlotImageForWorkflowRun(settings testSlotCIImageSettings, run githubWorkflowRun, sha string) (ResolvedTestSlotImage, error) {
	tag, err := ciLookupTagForWorkflowRun(run, sha)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	return resolvedTestSlotImageFromRepositoryTag(settings.Registry, settings.Repository, tag, fmt.Sprintf("github_actions:%s:run:%d:attempt:%d", settings.Workflow, run.ID, run.RunAttempt))
}

func ciLookupTagForWorkflowRun(run githubWorkflowRun, sha string) (string, error) {
	if run.ID <= 0 {
		return "", fmt.Errorf("workflow run id is required")
	}
	attempt := run.RunAttempt
	if attempt <= 0 {
		attempt = 1
	}
	if run.Event == "pull_request" && len(run.PullRequests) > 0 && run.PullRequests[0].Number > 0 {
		return fmt.Sprintf("ci-pr-%d-run-%d-attempt-%d", run.PullRequests[0].Number, run.ID, attempt), nil
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("source sha is required for non-PR CI lookup tag")
	}
	return fmt.Sprintf("ci-ref-%s-run-%d-attempt-%d", shortRefHash(sha), run.ID, attempt), nil
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
