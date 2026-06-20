package server

import (
	"context"
	"encoding/json"
	"errors"
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

// errTestSlotCIImageNotFound is returned (wrapped) by the validator when the
// resolved tag is absent from the registry. The resolver matches it with
// errors.Is to switch from "deploy this image" to "explain why it is missing"
// (build still running / failed / never ran), rather than surfacing a bare 404.
var errTestSlotCIImageNotFound = errors.New("image tag not found in registry")

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

// commitImageTag is the commit-addressed alias docker-build-check publishes,
// pointing at the content-fingerprinted manifest (app-<fingerprint>). The commit
// SHA is the key Glimmung already holds for the verified head, so resolution is a
// direct registry lookup with no GitHub Actions run reconstruction.
func commitImageTag(sha string) string {
	return "sha-" + strings.ToLower(strings.TrimSpace(sha))
}

// resolveTestSlotImageFromGitHubActions resolves a verified commit SHA to the
// CI-built image to deploy. docker-build-check publishes a commit-addressed
// `sha-<commit>` alias of the fingerprinted manifest, so resolution is a direct
// ACR lookup of that tag. GitHub Actions is consulted only to EXPLAIN a missing
// alias (build pending / failed / never ran) — never to reconstruct the tag.
func resolveTestSlotImageFromGitHubActions(ctx context.Context, httpClient *http.Client, project Project, slug, sha, token string, validator testSlotImageValidator) (ResolvedTestSlotImage, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("commit sha is required")
	}
	if strings.TrimSpace(slug) == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("github repo slug is required")
	}
	settings := testSlotCIImageConfig(project)

	resolved, err := resolvedTestSlotImageFromRepositoryTag(
		settings.Registry,
		settings.Repository,
		commitImageTag(sha),
		fmt.Sprintf("github_actions:%s:commit:%s", settings.Workflow, sha),
	)
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}

	switch validationErr := validator.ValidateTestSlotImage(ctx, resolved); {
	case validationErr == nil:
		return resolved, nil
	case errors.Is(validationErr, errTestSlotCIImageNotFound):
		// The alias is not in the registry. Read the commit's docker-build-check
		// run to explain why, so the caller gets an actionable pending/failed/
		// not-built signal instead of a bare 404.
		return ResolvedTestSlotImage{}, diagnoseMissingCommitImage(ctx, httpClient, slug, settings, sha, token)
	default:
		return ResolvedTestSlotImage{}, fmt.Errorf("validate CI image %s: %w", resolved.Image, validationErr)
	}
}

// diagnoseMissingCommitImage explains an absent `sha-<commit>` alias from the
// commit's docker-build-check runs: an in-progress build is retryable (the alias
// lands when the run reaches its "Tag image by commit" step), a failed build is
// terminal, and "succeeded but no alias" / "no run" name the remaining gaps.
func diagnoseMissingCommitImage(ctx context.Context, httpClient *http.Client, slug string, settings testSlotCIImageSettings, sha, token string) error {
	tag := commitImageTag(sha)
	runs, err := listWorkflowRunsForCommit(ctx, httpClient, slug, settings.Workflow, sha, token)
	if err != nil {
		return fmt.Errorf("no CI image for commit %s (tag %s absent); could not read %s run status: %w", sha, tag, settings.Workflow, err)
	}
	if run, ok := firstWorkflowRun(runs, workflowRunInProgress); ok {
		return &testSlotCIImagePendingError{
			SHA:      sha,
			Workflow: settings.Workflow,
			RunID:    run.ID,
			Status:   strings.TrimSpace(run.Status),
		}
	}
	if run, ok := firstWorkflowRun(runs, workflowRunSucceeded); ok {
		return fmt.Errorf("%s run %d for commit %s succeeded but published no %s tag; re-run the build to publish the CI image", settings.Workflow, run.ID, sha, tag)
	}
	if run, ok := firstWorkflowRun(runs, workflowRunFailed); ok {
		conclusion := strings.TrimSpace(run.Conclusion)
		if conclusion == "" {
			conclusion = "unsuccessfully"
		}
		return fmt.Errorf("%s run %d for commit %s concluded %s; no CI image was published — fix CI and redeploy", settings.Workflow, run.ID, sha, conclusion)
	}
	return fmt.Errorf("no CI image for commit %s in workflow %s: no build targets this commit (tag %s absent)", sha, settings.Workflow, tag)
}

// testSlotCIImagePendingError signals that a docker-build-check run targets the
// commit but has not published its CI image yet (the run is queued or in
// progress). It is retryable: the alias lands when the run completes, so
// deployImageToTestSlot maps it to 409 with an actionable message rather than a
// terminal 422. errors.As lets the caller branch on it without string-matching.
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
// slot hits when requested in the seconds between PR open and the build's
// commit-tag step. Any non-terminal status (queued, in_progress, requested,
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
// is the commit, at any status, so a missing image can be explained: a published
// alias (successful run), a build still running (retryable), or one that failed
// (terminal). It is used only by the miss-diagnostic, never to resolve the image.
func listWorkflowRunsForCommit(ctx context.Context, httpClient *http.Client, slug, workflow, sha, token string) ([]githubWorkflowRun, error) {
	values := url.Values{}
	values.Set("per_page", "20")
	values.Set("head_sha", sha)
	return fetchWorkflowRuns(ctx, httpClient, slug, workflow, token, values)
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
		return fmt.Errorf("%w: %s", errTestSlotCIImageNotFound, image.Image)
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
