package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type spyRunnerGitHubTokenMinter struct {
	token       string
	err         error
	calledRepo  string
	calledPerms map[string]string
}

func (m *spyRunnerGitHubTokenMinter) InstallationToken(ctx context.Context) (string, error) {
	return m.token, m.err
}

func (m *spyRunnerGitHubTokenMinter) RepositoryInstallationToken(ctx context.Context, repo string, permissions map[string]string) (string, error) {
	m.calledRepo = repo
	m.calledPerms = permissions
	return m.token, m.err
}

func TestRunnerGitHubAgentTokenByCallbackTokenMintsRepoScopedToken(t *testing.T) {
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
	minter := &spyRunnerGitHubTokenMinter{token: "ghs_scoped"}
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/cb/run/github-agent-token", nil)
	req.SetPathValue("callback_token", "cb")
	rec := httptest.NewRecorder()

	runnerGitHubAgentTokenByCallbackToken(store, minter)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result RunnerGitHubAgentTokenResult
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
	// The App installation does not grant checks; requesting it makes
	// GitHub 422 the entire mint, so the handler must not ask for it
	// (ambience#167 run 7.1).
	if _, requested := minter.calledPerms["checks"]; requested {
		t.Fatalf("handler requested ungranted checks permission: %v", minter.calledPerms)
	}
	if len(minter.calledPerms) != 2 {
		t.Fatalf("handler requested unexpected permissions: %v", minter.calledPerms)
	}
}
