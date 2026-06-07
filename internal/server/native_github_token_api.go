package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type NativeGitHubTokenMinter interface {
	InstallationToken(ctx context.Context) (string, error)
}

func positivePathInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	value, err := strconv.Atoi(r.PathValue(name))
	if err != nil || value < 1 {
		writeProblem(w, http.StatusBadRequest, name+" must be a positive integer")
		return 0, false
	}
	return value, true
}

type NativeGitHubTokenResult struct {
	Token string `json:"token"`
}

type NativeGitHubPushPolicyTokenResult struct {
	Token  string `json:"token"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
}

type runNumberResolver interface {
	ReadRunByNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, error)
}

func nativeGitHubTokenByCallbackToken(store ReadStore, minter NativeGitHubTokenMinter) http.HandlerFunc {
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
		writeNativeGitHubToken(w, r, completionStore, minter, project, runID)
	}
}

func nativeGitHubPushPolicyTokenByCallbackToken(store ReadStore, signingKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		writeNativeGitHubPushPolicyToken(w, r, completionStore, signingKey, project, runID)
	}
}

func nativeGitHubTokenByNumber(store ReadStore, minter NativeGitHubTokenMinter) http.HandlerFunc {
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
		writeNativeGitHubToken(w, r, completionStore, minter, r.PathValue("project"), runID)
	}
}

func writeNativeGitHubToken(w http.ResponseWriter, r *http.Request, store RunCompletionStore, minter NativeGitHubTokenMinter, project, runID string) {
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
	writeJSON(w, http.StatusOK, NativeGitHubTokenResult{Token: token})
}

func writeNativeGitHubPushPolicyToken(w http.ResponseWriter, r *http.Request, store RunCompletionStore, signingKey, project, runID string) {
	signingKey = strings.TrimSpace(signingKey)
	if signingKey == "" {
		writeProblem(w, http.StatusServiceUnavailable, "GitHub push policy signing key not configured")
		return
	}
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
	branch := nativePushPolicyBranchNameForRun(run)
	if branch == "" {
		writeProblem(w, http.StatusConflict, "run cannot derive implementation branch")
		return
	}
	ref := "refs/heads/" + branch
	payload := map[string]any{
		"version":      1,
		"repo":         repo,
		"branch":       branch,
		"ref":          ref,
		"run_id":       run.ID,
		"run_ref":      runRefFromData(run),
		"project":      run.Project,
		"issue_number": run.IssueNumber,
		"expires_at":   time.Now().Add(12 * time.Hour).Unix(),
	}
	token := signedGitHubPolicyToken(signingKey, payload)
	if token == "" {
		writeInternalError(w, r, errors.New("signed GitHub policy token was empty"), "mint GitHub push policy token failed")
		return
	}
	writeJSON(w, http.StatusOK, NativeGitHubPushPolicyTokenResult{
		Token:  token,
		Repo:   repo,
		Branch: branch,
		Ref:    ref,
	})
}

func nativePushPolicyBranchNameForRun(run RunReplayData) string {
	if run.IssueNumber > 0 && strings.TrimSpace(run.ID) != "" {
		return "glimmung/issue-" + nativePolicyRefSegment(strconv.Itoa(run.IssueNumber)) + "/" + nativePolicyRefSegment(run.ID)
	}
	if strings.TrimSpace(run.ID) != "" {
		return "glimmung/" + nativePolicyRefSegment(run.ID)
	}
	return ""
}
