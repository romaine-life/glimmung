package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// hotSwapDiff is the base/head diff context glimmung resolves for a
// project's hot-swap fidelity classifier.
//
// The classifier runs inside the build Job, in the cloned repo. That
// checkout is a shallow single-commit fetch of git_ref (FETCH_HEAD): it
// has no parent commit and no base remote-tracking ref, so an in-container
// `git diff base...HEAD` yields nothing and the classifier sees an empty
// change set ("no runtime artifact changed") regardless of what the branch
// actually touched. glimmung therefore computes the changed-file set
// authoritatively here — merge-base three-dot semantics via the GitHub
// Compare API, which is exact even when head is a raw SHA (the
// restricted-mode HEAD pin) — and passes it to the classifier as
// GLIMMUNG_CHANGED_FILES so the fidelity gate is meaningful.
type hotSwapDiff struct {
	BaseRef      string
	HeadRef      string
	ChangedFiles []string
	// Truncated is set when the Compare API hit its per-response file cap,
	// so the caller can record that the change set may be incomplete rather
	// than silently presenting a capped list as the whole diff.
	Truncated bool
}

// githubCompareFileCap is the GitHub Compare API's documented maximum number
// of files returned in a single comparison response. A result at the cap is
// reported as possibly-truncated.
const githubCompareFileCap = 300

// githubAPIBase is the GitHub REST API root. A package var (not a const) so
// tests can point it at an httptest server.
var githubAPIBase = "https://api.github.com"

// resolveHotSwapDiff resolves the base ref (explicit when supplied, else the
// repository's default branch) and the changed-file set between base and head
// using the already-minted installation token. A non-nil error means the diff
// could not be computed; the caller falls back to letting the in-container
// classifier diff against the base ref it can reach.
func resolveHotSwapDiff(ctx context.Context, httpClient *http.Client, slug, baseRef, headRef, token string) (hotSwapDiff, error) {
	slug = strings.TrimSpace(slug)
	headRef = strings.TrimSpace(headRef)
	baseRef = strings.TrimSpace(baseRef)
	out := hotSwapDiff{BaseRef: baseRef, HeadRef: headRef}
	if slug == "" || headRef == "" || strings.TrimSpace(token) == "" {
		return out, fmt.Errorf("slug, head ref, and token are required to resolve hot-swap diff")
	}
	if baseRef == "" {
		branch, err := githubDefaultBranch(ctx, httpClient, slug, token)
		if err != nil {
			return out, fmt.Errorf("resolve default branch: %w", err)
		}
		baseRef = branch
		out.BaseRef = branch
	}
	if baseRef == headRef {
		// Comparing a ref against itself is a no-op diff; skip the API call.
		return out, nil
	}
	files, truncated, err := githubCompareFiles(ctx, httpClient, slug, baseRef, headRef, token)
	if err != nil {
		return out, err
	}
	out.ChangedFiles = files
	out.Truncated = truncated
	return out, nil
}

func githubDefaultBranch(ctx context.Context, httpClient *http.Client, slug, token string) (string, error) {
	var payload struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := githubGetJSON(ctx, httpClient, githubAPIBase+"/repos/"+slug, token, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.DefaultBranch) == "" {
		return "", fmt.Errorf("repo %s has no default_branch", slug)
	}
	return payload.DefaultBranch, nil
}

func githubCompareFiles(ctx context.Context, httpClient *http.Client, slug, base, head, token string) ([]string, bool, error) {
	// three-dot: files changed on head relative to the merge-base with base.
	// This matches what the classifier's `git diff base...HEAD` would compute
	// with full history, and is correct when head is a raw SHA.
	basehead := url.PathEscape(base) + "..." + url.PathEscape(head)
	apiURL := githubAPIBase + "/repos/" + slug + "/compare/" + basehead + "?per_page=100"
	var payload struct {
		Status string `json:"status"`
		Files  []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := githubGetJSON(ctx, httpClient, apiURL, token, &payload); err != nil {
		return nil, false, err
	}
	files := make([]string, 0, len(payload.Files))
	for _, f := range payload.Files {
		if name := strings.TrimSpace(f.Filename); name != "" {
			files = append(files, name)
		}
	}
	return files, len(payload.Files) >= githubCompareFileCap, nil
}

func githubGetJSON(ctx context.Context, httpClient *http.Client, apiURL, token string, out any) error {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("github GET %s returned %d: %s", apiURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode github response: %w", err)
	}
	return nil
}
