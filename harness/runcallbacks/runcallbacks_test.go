package runcallbacks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

func TestMintGitHubTokenHappyPath(t *testing.T) {
	var gotToken, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Glimmung-Attempt-Token")
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"token":"ghs_minted_abc"}`))
	}))
	defer srv.Close()

	cfg := Config{GitHubTokenURL: srv.URL, AttemptToken: "tok-abc", HTTPClient: srv.Client()}
	token, lerr := cfg.MintGitHubToken(context.Background())
	if lerr != nil {
		t.Fatalf("MintGitHubToken: %v", lerr)
	}
	if token != "ghs_minted_abc" {
		t.Fatalf("token = %q, want ghs_minted_abc", token)
	}
	if gotToken != "tok-abc" {
		t.Fatalf("attempt-token header = %q, want tok-abc", gotToken)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
}

func TestMintGitHubTokenMisconfiguredIsHarnessLayer(t *testing.T) {
	for _, cfg := range []Config{
		{AttemptToken: "tok"},                        // no URL
		{GitHubTokenURL: "https://example/callback"}, // no attempt token
	} {
		_, lerr := cfg.MintGitHubToken(context.Background())
		if lerr == nil || lerr.Layer != steperr.LayerHarness {
			t.Fatalf("missing wire must be a harness misconfiguration, got %v", lerr)
		}
	}
}

func TestMintGitHubTokenEndpointErrorIsHostLayer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := Config{GitHubTokenURL: srv.URL, AttemptToken: "tok", HTTPClient: srv.Client()}
	_, lerr := cfg.MintGitHubToken(context.Background())
	if lerr == nil || lerr.Layer != steperr.LayerHost {
		t.Fatalf("a 5xx callback is a host-layer venue failure, got %v", lerr)
	}
	if !strings.Contains(lerr.Message, "HTTP 500") {
		t.Fatalf("error should carry the status, got %q", lerr.Message)
	}
}

func TestMintGitHubTokenInvalidJSONIsHarnessLayer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	cfg := Config{GitHubTokenURL: srv.URL, AttemptToken: "tok", HTTPClient: srv.Client()}
	_, lerr := cfg.MintGitHubToken(context.Background())
	if lerr == nil || lerr.Layer != steperr.LayerHarness {
		t.Fatalf("malformed callback response is a harness error, got %v", lerr)
	}
}

func TestMintGitHubTokenEmptyTokenIsHarnessLayer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token":"   "}`))
	}))
	defer srv.Close()
	cfg := Config{GitHubTokenURL: srv.URL, AttemptToken: "tok", HTTPClient: srv.Client()}
	_, lerr := cfg.MintGitHubToken(context.Background())
	if lerr == nil || lerr.Layer != steperr.LayerHarness {
		t.Fatalf("empty token must be rejected as a harness error, got %v", lerr)
	}
}
