package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type ReviewStore interface {
	ListReviews(ctx context.Context, filter ReviewListFilter) ([]ReviewRow, error)
	GetReviewForIssue(ctx context.Context, project string, issueNumber int) (ReviewDetail, error)
	EnsureReview(ctx context.Context, req ReviewCreate) (ReviewDetail, error)
}

type ReviewListFilter struct {
	Project string
	Repo    string
	State   string
	Limit   *int
}

type ReviewRow struct {
	Ref                  string               `json:"ref"`
	Project              string               `json:"project"`
	Repo                 string               `json:"repo"`
	PRNumber             int                  `json:"pr_number"`
	PRBranch             *string              `json:"pr_branch"`
	Title                string               `json:"title"`
	State                string               `json:"state"`
	Merged               bool                 `json:"merged"`
	HTMLURL              *string              `json:"html_url"`
	LinkedIssueRef       *string              `json:"linked_issue_ref"`
	LinkedRunRef         *string              `json:"linked_run_ref"`
	IssueNumber          *int                 `json:"issue_number"`
	RunRef               *string              `json:"run_ref"`
	RunState             *string              `json:"run_state"`
	ValidationURL        *string              `json:"validation_url"`
	SessionLaunchURL     *string              `json:"session_launch_url"`
	RunAttempts          int                  `json:"run_attempts"`
	RunCumulativeCostUSD float64              `json:"run_cumulative_cost_usd"`
	PRLockHeld           bool                 `json:"pr_lock_held"`
	Evidence             []ReviewEvidence `json:"evidence"`
}

type ReviewDetail struct {
	Ref                  string               `json:"ref"`
	Project              string               `json:"project"`
	Repo                 string               `json:"repo"`
	PRNumber             int                  `json:"pr_number"`
	PRBranch             *string              `json:"pr_branch"`
	Title                string               `json:"title"`
	Body                 string               `json:"body"`
	State                string               `json:"state"`
	Merged               bool                 `json:"merged"`
	BaseRef              string               `json:"base_ref"`
	HeadSHA              string               `json:"head_sha"`
	HTMLURL              *string              `json:"html_url"`
	LinkedIssueRef       *string              `json:"linked_issue_ref"`
	LinkedRunRef         *string              `json:"linked_run_ref"`
	IssueNumber          *int                 `json:"issue_number"`
	IssueTitle           *string              `json:"issue_title"`
	RunRef               *string              `json:"run_ref"`
	RunState             *string              `json:"run_state"`
	ValidationURL        *string              `json:"validation_url"`
	ScreenshotsMarkdown  *string              `json:"screenshots_markdown"`
	SessionLaunchURL     *string              `json:"session_launch_url"`
	RunAttempts          int                  `json:"run_attempts"`
	RunCumulativeCostUSD float64              `json:"run_cumulative_cost_usd"`
	RunAttemptHistory    []map[string]any     `json:"run_attempt_history"`
	Comments             []map[string]any     `json:"comments"`
	Reviews              []map[string]any     `json:"reviews"`
	PRLockHeld           bool                 `json:"pr_lock_held"`
	Evidence             []ReviewEvidence `json:"evidence"`
}

type ReviewCreate struct {
	Project        string
	Repo           string
	Number         int
	Title          string
	Branch         string
	Body           string
	BaseRef        string
	HeadSHA        string
	HTMLURL        string
	LinkedIssueRef string
	LinkedRunRef   string
	Evidence       []ReviewEvidence
	EvidenceSet    bool
}

type ReviewCreateRequest struct {
	Project        string               `json:"project"`
	Repo           string               `json:"repo"`
	Number         int                  `json:"number"`
	Title          string               `json:"title"`
	Branch         string               `json:"branch"`
	Body           string               `json:"body"`
	BaseRef        string               `json:"base_ref"`
	HeadSHA        string               `json:"head_sha"`
	HTMLURL        string               `json:"html_url"`
	LinkedIssueRef *string              `json:"linked_issue_ref"`
	LinkedRunRef   *string              `json:"linked_run_ref"`
	Evidence       []ReviewEvidence `json:"evidence,omitempty"`
}

type ReviewEvidence = EvidenceArtifact

func listReviews(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tpStore, ok := store.(ReviewStore)
		if !ok || tpStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "review store not configured")
			return
		}
		limit, ok := parseOptionalIssueLimit(w, r)
		if !ok {
			return
		}
		filter := ReviewListFilter{
			Project: r.URL.Query().Get("project"),
			Repo:    r.URL.Query().Get("repo"),
			State:   r.URL.Query().Get("state"),
			Limit:   limit,
		}
		rows, err := tpStore.ListReviews(r.Context(), filter)
		if err != nil {
			writeInternalError(w, r, err, "list reviews failed")
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

func issueReviewDetail(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tpStore, ok := store.(ReviewStore)
		if !ok || tpStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "review store not configured")
			return
		}
		issueNumber, err := strconv.Atoi(r.PathValue("issue_number"))
		if err != nil || issueNumber < 1 {
			writeProblem(w, http.StatusBadRequest, "issue_number must be a positive integer")
			return
		}
		detail, err := tpStore.GetReviewForIssue(r.Context(), r.PathValue("project"), issueNumber)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusNotFound, "review not found")
			return
		case err != nil:
			writeInternalError(w, r, err, "get review failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func createReview(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tpStore, ok := store.(ReviewStore)
		if !ok || tpStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "review store not configured")
			return
		}
		var body ReviewCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if body.Project == "" {
			writeProblem(w, http.StatusBadRequest, "project required")
			return
		}
		if body.Repo == "" {
			writeProblem(w, http.StatusBadRequest, "repo required")
			return
		}
		if body.Number < 1 {
			writeProblem(w, http.StatusBadRequest, "number must be a positive integer")
			return
		}
		if strings.TrimSpace(body.Title) == "" {
			writeProblem(w, http.StatusBadRequest, "title required")
			return
		}
		if strings.TrimSpace(body.Branch) == "" {
			writeProblem(w, http.StatusBadRequest, "branch required")
			return
		}
		req := ReviewCreate{
			Project: body.Project,
			Repo:    body.Repo,
			Number:  body.Number,
			Title:   body.Title,
			Branch:  body.Branch,
			Body:    body.Body,
			BaseRef: firstNonEmpty(body.BaseRef, "main"),
			HeadSHA: body.HeadSHA,
			HTMLURL: body.HTMLURL,
		}
		if len(body.Evidence) > 0 {
			req.Evidence = body.Evidence
			req.EvidenceSet = true
		}
		if body.LinkedIssueRef != nil {
			req.LinkedIssueRef = *body.LinkedIssueRef
		}
		if body.LinkedRunRef != nil {
			req.LinkedRunRef = *body.LinkedRunRef
		}
		detail, err := tpStore.EnsureReview(r.Context(), req)
		switch {
		case errors.Is(err, ErrNotFound):
			writeProblem(w, http.StatusBadRequest, "referenced project not found")
			return
		case err != nil:
			writeInternalError(w, r, err, "ensure review failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}
