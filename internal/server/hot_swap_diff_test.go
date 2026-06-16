package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveHotSwapDiffComputesChangedFiles pins the issue-3 fix: glimmung
// resolves the changed-file set via the GitHub Compare API (three-dot,
// merge-base) using the minted token, defaulting the base to the repo's default
// branch — exactly the context the build Job's shallow checkout cannot compute.
func TestResolveHotSwapDiffComputesChangedFiles(t *testing.T) {
	var sawAuth, sawComparePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/repos/romaine-life/tank-operator":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case strings.HasPrefix(r.URL.Path, "/repos/romaine-life/tank-operator/compare/"):
			sawComparePath = r.URL.Path
			_, _ = w.Write([]byte(`{"status":"ahead","files":[{"filename":"frontend/src/App.tsx"},{"filename":"backend-go/cmd/tank-operator/server.go"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	old := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = old }()

	diff, err := resolveHotSwapDiff(context.Background(), srv.Client(), "romaine-life/tank-operator", "", "feat/x", "ghs_token")
	if err != nil {
		t.Fatalf("resolveHotSwapDiff: %v", err)
	}
	if diff.BaseRef != "main" {
		t.Fatalf("base ref = %q, want main (default branch)", diff.BaseRef)
	}
	if len(diff.ChangedFiles) != 2 {
		t.Fatalf("changed files = %v, want 2", diff.ChangedFiles)
	}
	if diff.ChangedFiles[0] != "frontend/src/App.tsx" {
		t.Fatalf("changed files = %v, want frontend/src/App.tsx first", diff.ChangedFiles)
	}
	if sawAuth != "Bearer ghs_token" {
		t.Fatalf("compare call auth = %q, want bearer token", sawAuth)
	}
	// three-dot merge-base comparison in the path.
	if !strings.Contains(sawComparePath, "main...feat/x") {
		t.Fatalf("compare path = %q, want main...feat/x", sawComparePath)
	}
}

// TestResolveHotSwapDiffSameRefSkipsCompare pins that comparing a ref against
// itself short-circuits to an empty (but non-error) diff.
func TestResolveHotSwapDiffSameRefSkipsCompare(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	}))
	defer srv.Close()
	old := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = old }()

	diff, err := resolveHotSwapDiff(context.Background(), srv.Client(), "owner/repo", "main", "main", "tok")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(diff.ChangedFiles) != 0 {
		t.Fatalf("changed files = %v, want empty", diff.ChangedFiles)
	}
	if called {
		t.Fatal("compare API should not be called when base == head")
	}
}

func TestResolveHotSwapDiffRequiresInputs(t *testing.T) {
	if _, err := resolveHotSwapDiff(context.Background(), nil, "", "main", "head", "tok"); err == nil {
		t.Fatal("missing slug should error")
	}
	if _, err := resolveHotSwapDiff(context.Background(), nil, "o/r", "main", "head", ""); err == nil {
		t.Fatal("missing token should error")
	}
}
