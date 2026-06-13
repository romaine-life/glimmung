package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

type RunnerGitHubTokenMinter interface {
	InstallationToken(ctx context.Context) (string, error)
	RepositoryInstallationToken(ctx context.Context, repo string, permissions map[string]string) (string, error)
}

func positivePathInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	value, err := strconv.Atoi(r.PathValue(name))
	if err != nil || value < 1 {
		writeProblem(w, http.StatusBadRequest, name+" must be a positive integer")
		return 0, false
	}
	return value, true
}

type RunnerGitHubTokenResult struct {
	Token string `json:"token"`
}

type RunnerGitHubAgentTokenResult struct {
	Token string `json:"token"`
	Repo  string `json:"repo"`
}

type runNumberResolver interface {
	ReadRunByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, error)
}

func runnerGitHubTokenByCallbackToken(store ReadStore, minter RunnerGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "GitHub token minter not configured")
			return
		}
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		runID, project, _, err := completionStore.ReadRunIDForCallbackToken(r.Context(), r.PathValue("callback_token"))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run by callback token failed")
			return
		}
		writeRunnerGitHubToken(w, r, completionStore, minter, project, runID)
	}
}

func runnerGitHubAgentTokenByCallbackToken(store ReadStore, minter RunnerGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "GitHub token minter not configured")
			return
		}
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		runID, project, _, err := completionStore.ReadRunIDForCallbackToken(r.Context(), r.PathValue("callback_token"))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run callback token not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run by callback token failed")
			return
		}
		writeRunnerGitHubAgentToken(w, r, completionStore, minter, project, runID)
	}
}

func runnerGitHubTokenByNumber(store ReadStore, minter RunnerGitHubTokenMinter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if minter == nil {
			writeProblem(w, http.StatusServiceUnavailable, "GitHub token minter not configured")
			return
		}
		completionStore, ok := store.(RunCompletionStore)
		if !ok || completionStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run store not configured")
			return
		}
		runResolver, ok := store.(runNumberResolver)
		if !ok || runResolver == nil {
			writeProblem(w, http.StatusServiceUnavailable, "run lookup store not configured")
			return
		}
		issueNumber, ok := positivePathInt(w, r, "issue_number")
		if !ok {
			return
		}
		runID, err := runResolver.ReadRunByNumber(r.Context(), r.PathValue("project"), issueNumber, r.PathValue("run_number"))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "read run failed")
			return
		}
		writeRunnerGitHubToken(w, r, completionStore, minter, r.PathValue("project"), runID)
	}
}

func writeRunnerGitHubToken(w http.ResponseWriter, r *http.Request, store RunCompletionStore, minter RunnerGitHubTokenMinter, project, runID string) {
	run, err := store.ReadRunForReplay(r.Context(), project, runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return
	}
	if run.IssueRepo == "" {
		writeProblem(w, http.StatusConflict, "run has no issue repo")
		return
	}
	token, err := minter.InstallationToken(r.Context())
	if err != nil {
		writeProblem(w, http.StatusBadGateway, "mint GitHub token failed")
		return
	}
	writeJSON(w, http.StatusOK, RunnerGitHubTokenResult{Token: token})
}

func writeRunnerGitHubAgentToken(w http.ResponseWriter, r *http.Request, store RunCompletionStore, minter RunnerGitHubTokenMinter, project, runID string) {
	run, err := store.ReadRunForReplay(r.Context(), project, runID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeInternalError(w, r, err, "read run failed")
		return
	}
	repo := strings.TrimSpace(run.IssueRepo)
	if repo == "" {
		writeProblem(w, http.StatusConflict, "run has no issue repo")
		return
	}
	// Request exactly the permissions the App installation holds and the
	// agent contract needs. The installation grants {issues, actions,
	// contents, metadata, workflows, pull_requests}; requesting anything
	// outside the grant makes GitHub 422 the whole mint ("The permissions
	// requested are not granted to this installation") — the prior set
	// included checks:read and had never minted successfully (ambience#167
	// run 7.1). CI check-runs are readable without checks:read on public
	// repos; if a private project onboards, grant checks:read on the App
	// first and then widen this set.
	permissions := map[string]string{
		"contents": "write",
		"metadata": "read",
	}
	token, err := minter.RepositoryInstallationToken(r.Context(), repo, permissions)
	if err != nil {
		slog.Error("mint repo-scoped GitHub agent token failed",
			"repo", repo,
			"error", err,
		)
		writeProblem(w, http.StatusBadGateway, "mint repo-scoped GitHub token failed")
		return
	}
	writeJSON(w, http.StatusOK, RunnerGitHubAgentTokenResult{
		Token: token,
		Repo:  repo,
	})
}
