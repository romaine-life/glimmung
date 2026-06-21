// Package runcallbacks promotes Glimmung's per-attempt run callbacks into the
// SDK. It mints the per-attempt GitHub token from the run callback URL Glimmung
// bakes onto the pod (GLIMMUNG_GITHUB_TOKEN_URL, authenticated with the
// X-Glimmung-Attempt-Token header).
//
// It is the venue-independent companion to harness/remotehost's ssh-cert and
// tailscale-authkey mints: the token mint is NOT remote-host specific — an
// in-cluster consumer (ambience) needs the GitHub token without importing the
// remote-host venue at all — so it lives in its own package rather than coupling
// every consumer to remotehost.
//
// The callback is the ONLY sanctioned source of the token. A runner Job must
// never mount the real provider OAuth secret (see the repo's
// provider-api-proxy-auth contract); it mints a short-lived token per attempt
// through this callback instead.
//
// Layering is honest: a missing wire (URL / attempt token) or a malformed
// callback response is a harness misconfiguration (step.LayerHarness); a
// callback endpoint that is unreachable or returns an error status is a venue
// failure (step.LayerHost). That keeps a misconfigured pod from being blamed on
// the host, and a down callback from being blamed on the harness.
package runcallbacks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/harness/step"
)

// Config is the GitHub-token mint wiring, normally built from the GLIMMUNG_*
// environment via FromContext. HTTPClient is injectable so the mint is testable
// against an httptest server with no real callback.
type Config struct {
	GitHubTokenURL string       // GLIMMUNG_GITHUB_TOKEN_URL
	AttemptToken   string       // GLIMMUNG_ATTEMPT_TOKEN — X-Glimmung-Attempt-Token header
	HTTPClient     *http.Client // nil uses a 30s-timeout default
}

// FromContext builds a Config from a step.Context's GLIMMUNG_* environment.
func FromContext(c *step.Context) Config {
	return Config{
		GitHubTokenURL: strings.TrimSpace(c.Env("GLIMMUNG_GITHUB_TOKEN_URL")),
		AttemptToken:   strings.TrimSpace(c.Env("GLIMMUNG_ATTEMPT_TOKEN")),
	}
}

func (cfg Config) httpClient() *http.Client {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// MintGitHubToken POSTs to the run callback and returns the per-attempt GitHub
// token from the response's `token` field.
func (cfg Config) MintGitHubToken(ctx context.Context) (string, *step.LayeredError) {
	if cfg.GitHubTokenURL == "" || cfg.AttemptToken == "" {
		return "", step.HarnessError("github_token_misconfigured", "GLIMMUNG_GITHUB_TOKEN_URL / GLIMMUNG_ATTEMPT_TOKEN not set", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GitHubTokenURL, nil)
	if err != nil {
		return "", step.HarnessError("github_token_request", "build github token request", err)
	}
	req.Header.Set("X-Glimmung-Attempt-Token", cfg.AttemptToken)
	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return "", step.HostError("github_token_request", "github token endpoint request failed", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode >= 400 {
		return "", step.HostError("github_token_request", fmt.Sprintf("github token endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(buf.String())), nil)
	}
	var doc struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		return "", step.HarnessError("github_token_request", "github token endpoint returned invalid JSON", err)
	}
	if strings.TrimSpace(doc.Token) == "" {
		return "", step.HarnessError("github_token_request", "github token endpoint returned no usable .token", nil)
	}
	return strings.TrimSpace(doc.Token), nil
}
