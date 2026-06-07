package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type spyNativeGitHubTokenMinter struct {
	token       string
	err         error
	calledRepo  string
	calledPerms map[string]string
}

func (m *spyNativeGitHubTokenMinter) InstallationToken(ctx context.Context) (string, error) {
	return m.token, m.err
}

func (m *spyNativeGitHubTokenMinter) RepositoryInstallationToken(ctx context.Context, repo string, permissions map[string]string) (string, error) {
	m.calledRepo = repo
	m.calledPerms = permissions
	return m.token, m.err
}

func TestNativeGitHubAgentTokenByCallbackTokenMintsRepoScopedToken(t *testing.T) {
	store := &fakeCompletionStore{
		tokenRunID:   "run-abc",
		tokenProject: "ambience",
		run: &RunReplayData{
			ID:          "run-abc",
			Project:     "ambience",
			IssueNumber: 168,
			IssueRepo:   "romaine-life/ambience",
		},
	}
	minter := &spyNativeGitHubTokenMinter{token: "ghs_scoped"}
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/cb/native/github-agent-token", nil)
	req.SetPathValue("callback_token", "cb")
	rec := httptest.NewRecorder()

	nativeGitHubAgentTokenByCallbackToken(store, minter)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result NativeGitHubAgentTokenResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Token != "ghs_scoped" {
		t.Fatalf("token=%q, want ghs_scoped", result.Token)
	}
	if result.Repo != "romaine-life/ambience" {
		t.Fatalf("repo=%q, want romaine-life/ambience", result.Repo)
	}
	if minter.calledRepo != "romaine-life/ambience" {
		t.Fatalf("minter called repo=%q", minter.calledRepo)
	}
	if minter.calledPerms["contents"] != "write" || minter.calledPerms["metadata"] != "read" {
		t.Fatalf("minter called perms=%v", minter.calledPerms)
	}
}
