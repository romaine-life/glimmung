package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ServiceTokenSource yields an auth.romaine.life service-principal bearer token
// for an outbound call. It is the client side of the auth model the edge's
// GET /__live-preview/status enforces (a valid auth.romaine.life token of any
// accepted role): Glimmung's control plane reads the edge back as a service
// principal.
type ServiceTokenSource interface {
	Token(ctx context.Context) (string, error)
}

// romaineServiceExchangePath is the auth.romaine.life endpoint that exchanges a
// projected k8s SA token (audience https://auth.romaine.life) for a
// service-principal JWT (role=service). Same endpoint the runner launcher's
// Tank session-scope retire exchange uses.
const romaineServiceExchangePath = "/api/auth/exchange/k8s"

// RomaineServiceTokenSource mints auth.romaine.life service-principal JWTs by
// reading the pod's projected SA token from disk and exchanging it. It caches
// the exchanged JWT until shortly before expiry so a tight verifier poll loop
// does not hammer the exchange endpoint (cost story: one exchange per cache
// window, not per read-back).
//
// It mirrors KubernetesRunLauncher.exchangeAuthRomaineServiceToken's federation
// pattern (read SA token, POST as Bearer, take the returned `token`) but is a
// standalone, cacheable source the preview verifier depends on without coupling
// to the runner launcher.
type RomaineServiceTokenSource struct {
	AuthBaseURL string
	TokenPath   string
	HTTP        *http.Client
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
	// ttl is how long an exchanged token is reused before re-exchange. auth's
	// service JWTs are short-lived; this is a conservative reuse window with
	// headroom before the real expiry.
	ttl time.Duration

	mu        sync.Mutex
	cached    string
	cachedExp time.Time
}

// NewRomaineServiceTokenSource constructs a cacheable service-token source.
// Returns nil when unconfigured (no base URL or token path) so callers can
// detect the disabled state and fail closed rather than emit unauthenticated
// reads.
func NewRomaineServiceTokenSource(authBaseURL, tokenPath string, httpClient *http.Client) *RomaineServiceTokenSource {
	authBaseURL = strings.TrimRight(strings.TrimSpace(authBaseURL), "/")
	tokenPath = strings.TrimSpace(tokenPath)
	if authBaseURL == "" || tokenPath == "" {
		return nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &RomaineServiceTokenSource{
		AuthBaseURL: authBaseURL,
		TokenPath:   tokenPath,
		HTTP:        httpClient,
		now:         time.Now,
		ttl:         5 * time.Minute,
	}
}

// Token returns a cached service JWT when one is still fresh, else exchanges the
// projected SA token for a new one.
func (s *RomaineServiceTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", fmt.Errorf("auth.romaine.life service token source not configured")
	}
	nowFn := s.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()

	s.mu.Lock()
	if s.cached != "" && now.Before(s.cachedExp) {
		token := s.cached
		s.mu.Unlock()
		return token, nil
	}
	s.mu.Unlock()

	token, err := s.exchange(ctx)
	if err != nil {
		return "", err
	}
	ttl := s.ttl
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	s.mu.Lock()
	s.cached = token
	s.cachedExp = now.Add(ttl)
	s.mu.Unlock()
	return token, nil
}

func (s *RomaineServiceTokenSource) exchange(ctx context.Context) (string, error) {
	data, err := os.ReadFile(s.TokenPath)
	if err != nil {
		return "", fmt.Errorf("read auth.romaine.life service account token: %w", err)
	}
	saToken := strings.TrimSpace(string(data))
	if saToken == "" {
		return "", fmt.Errorf("auth.romaine.life service account token is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.AuthBaseURL+romaineServiceExchangePath, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+saToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth.romaine.life service exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("auth.romaine.life service exchange returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("decode auth.romaine.life service exchange response: %w", err)
	}
	token := strings.TrimSpace(decoded.Token)
	if token == "" {
		return "", fmt.Errorf("auth.romaine.life service exchange response missing token")
	}
	return token, nil
}
