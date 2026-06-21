package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRomaineServiceTokenSourceExchangesAndCaches(t *testing.T) {
	var exchanges int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != romaineServiceExchangePath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sa-token-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&exchanges, 1)
		_, _ = w.Write([]byte(`{"token":"service-jwt-123"}`))
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := NewRomaineServiceTokenSource(srv.URL, tokenPath, srv.Client())
	if src == nil {
		t.Fatal("expected configured source")
	}
	// Pin time so the cache window is deterministic.
	base := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	src.now = func() time.Time { return base }

	for i := 0; i < 3; i++ {
		tok, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if tok != "service-jwt-123" {
			t.Fatalf("token = %q", tok)
		}
	}
	if got := atomic.LoadInt32(&exchanges); got != 1 {
		t.Fatalf("exchanges = %d, want 1 (cached within the TTL window)", got)
	}

	// Advance past the TTL -> a fresh exchange.
	src.now = func() time.Time { return base.Add(10 * time.Minute) }
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("token after ttl: %v", err)
	}
	if got := atomic.LoadInt32(&exchanges); got != 2 {
		t.Fatalf("exchanges = %d, want 2 (re-exchanged after TTL)", got)
	}
}

func TestRomaineServiceTokenSourceUnconfigured(t *testing.T) {
	if src := NewRomaineServiceTokenSource("", "/x", nil); src != nil {
		t.Fatalf("empty base URL must yield nil source")
	}
	if src := NewRomaineServiceTokenSource("https://auth", "", nil); src != nil {
		t.Fatalf("empty token path must yield nil source")
	}
}
