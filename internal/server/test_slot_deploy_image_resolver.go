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
