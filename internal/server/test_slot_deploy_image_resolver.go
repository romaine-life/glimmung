package server

import (
	"context"
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

func projectMetadataTestSlotImageResolver(validator testSlotImageValidator) testSlotImageResolver {
	if validator == nil {
		validator = noopTestSlotImageValidator{}
	}
	return func(ctx context.Context, project Project, sha string) (ResolvedTestSlotImage, error) {
		resolved, err := resolveTestSlotImageFromProjectMetadata(project, sha)
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		if err := validator.ValidateTestSlotImage(ctx, resolved); err != nil {
			return ResolvedTestSlotImage{}, err
		}
		return resolved, nil
	}
}

func resolveTestSlotImageFromProjectMetadata(project Project, sha string) (ResolvedTestSlotImage, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("commit sha is required")
	}
	raw, ok := testSlotDeployMetadata(project)
	if !ok {
		return ResolvedTestSlotImage{}, fmt.Errorf("project %s has no test_slot_deploy.ci_image resolver metadata", project.Name)
	}
	ciImage, ok := mapFromMap(raw, "ci_image")
	if !ok {
		ciImage, ok = mapFromMap(raw, "ciImage")
	}
	if !ok {
		return ResolvedTestSlotImage{}, fmt.Errorf("project %s has no test_slot_deploy.ci_image resolver metadata", project.Name)
	}

	imagesBySHA := stringMapFromAnyMap(anyMap(firstAny(ciImage["images_by_sha"], ciImage["imagesBySha"])))
	if image := strings.TrimSpace(imagesBySHA[sha]); image != "" {
		resolved, err := resolvedTestSlotImageFromRef(image, "project_metadata:test_slot_deploy.ci_image.images_by_sha")
		if err != nil {
			return ResolvedTestSlotImage{}, err
		}
		if tagIsRawCommitSHA(resolved.Tag, sha) {
			return ResolvedTestSlotImage{}, fmt.Errorf("resolved image tag %q is the raw commit SHA; test-slot deploy requires a fingerprinted CI image tag", resolved.Tag)
		}
		return resolved, nil
	}

	tagsBySHA := stringMapFromAnyMap(anyMap(firstAny(ciImage["tags_by_sha"], ciImage["tagsBySha"])))
	tag := strings.TrimSpace(tagsBySHA[sha])
	if tag == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("no CI image mapping for commit %s in test_slot_deploy.ci_image", sha)
	}
	if tagIsRawCommitSHA(tag, sha) {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image tag %q is the raw commit SHA; test-slot deploy requires a fingerprinted CI image tag", tag)
	}

	registry := configString(ciImage, "registry")
	repository := firstNonEmpty(
		configString(ciImage, "repository"),
		configString(ciImage, "image_repository", "imageRepository"),
	)
	resolved, err := resolvedTestSlotImageFromRepositoryTag(registry, repository, tag, "project_metadata:test_slot_deploy.ci_image.tags_by_sha")
	if err != nil {
		return ResolvedTestSlotImage{}, err
	}
	return resolved, nil
}

func testSlotDeployMetadata(project Project) (map[string]any, bool) {
	for _, key := range []string{"test_slot_deploy", "testSlotDeploy"} {
		if raw, ok := mapFromMap(project.Metadata, key); ok {
			return raw, true
		}
	}
	return nil, false
}

func resolvedTestSlotImageFromRepositoryTag(registry, repository, tag, source string) (ResolvedTestSlotImage, error) {
	repository = strings.TrimSpace(repository)
	tag = strings.TrimSpace(tag)
	if repository == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("test_slot_deploy.ci_image.repository is required when tags_by_sha is used")
	}
	if tag == "" {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image tag is required")
	}
	if strings.Contains(tag, "/") || strings.Contains(tag, ":") {
		return ResolvedTestSlotImage{}, fmt.Errorf("resolved image tag %q must be a tag, not an image ref", tag)
	}
	registry = strings.TrimSpace(registry)
	if registry == "" {
		if parsedRegistry, parsedRepository, ok := splitRegistryRepository(repository); ok {
			registry = parsedRegistry
			repository = parsedRepository
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

func tagIsRawCommitSHA(tag, sha string) bool {
	tag = strings.TrimSpace(strings.ToLower(tag))
	sha = strings.TrimSpace(strings.ToLower(sha))
	if tag == "" || sha == "" {
		return false
	}
	return tag == sha
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
