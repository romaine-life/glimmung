package store

import (
	"testing"
	"time"
)

// TestReviewRowBindsToOwnLinkedRun guards the dashboard's per-run review
// scoping. When one issue has several runs/PRs (e.g. a recycled or re-dispatched
// issue), each review row must resolve to the run named by its own
// linked_run_id — not the issue's latest run. Resolving by issue alone mis-binds
// every review on the issue to the newest run, which made the dashboard's
// pickDecisionReview route an Approve at the wrong (older) PR.
func TestReviewRowBindsToOwnLinkedRun(t *testing.T) {
	issueID := "issue-uuid-1"
	runs := []runDoc{
		{
			ID: "run-old", Project: "proj", IssueID: issueID,
			IssueRepo: "owner/repo", IssueNumber: 7, PRNumber: intPtr(101),
			State: "aborted", RunDisplayNumber: ptrString("1.1"),
			CreatedAt: "2026-01-01T00:00:00Z",
		},
		{
			ID: "run-new", Project: "proj", IssueID: issueID,
			IssueRepo: "owner/repo", IssueNumber: 7, PRNumber: intPtr(102),
			State: "review_required", RunDisplayNumber: ptrString("2.1"),
			CreatedAt: "2026-01-02T00:00:00Z",
		},
	}
	refByID, byID, byLinkedIssue, byRepoPR := buildRunIndexes(runs)

	oldReview := reviewDoc{
		Repo: "owner/repo", Number: 101, State: "merged",
		LinkedIssueID: ptrString(issueID), LinkedRunID: ptrString("run-old"),
	}
	newReview := reviewDoc{
		Repo: "owner/repo", Number: 102, State: "ready",
		LinkedIssueID: ptrString(issueID), LinkedRunID: ptrString("run-new"),
	}

	oldRow := reviewRowFromDoc(oldReview, nil, nil, refByID, byID, byLinkedIssue, byRepoPR, nil, time.Now().UTC())
	newRow := reviewRowFromDoc(newReview, nil, nil, refByID, byID, byLinkedIssue, byRepoPR, nil, time.Now().UTC())

	if got, want := derefString(oldRow.LinkedRunRef), refByID["run-old"]; got != want {
		t.Fatalf("review #101 should bind to run-old (%q), got %q", want, got)
	}
	if got := derefString(oldRow.RunState); got != "aborted" {
		t.Fatalf("review #101 run state want %q, got %q", "aborted", got)
	}
	if got, want := derefString(newRow.LinkedRunRef), refByID["run-new"]; got != want {
		t.Fatalf("review #102 should bind to run-new (%q), got %q", want, got)
	}
	if got := derefString(newRow.RunState); got != "review_required" {
		t.Fatalf("review #102 run state want %q, got %q", "review_required", got)
	}

	// Legacy review rows that predate linked_run_id keep falling back to the
	// issue's latest run, so older rows still resolve to a run.
	legacy := reviewDoc{Repo: "owner/repo", Number: 103, State: "ready", LinkedIssueID: ptrString(issueID)}
	legacyRow := reviewRowFromDoc(legacy, nil, nil, refByID, byID, byLinkedIssue, byRepoPR, nil, time.Now().UTC())
	if got, want := derefString(legacyRow.LinkedRunRef), refByID["run-new"]; got != want {
		t.Fatalf("legacy review (no linked_run_id) should fall back to the issue's latest run (%q), got %q", want, got)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
