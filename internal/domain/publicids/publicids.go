package publicids

import (
	"strconv"
	"strings"
)

// IssueRef returns the operator-facing issue reference for a project issue.
func IssueRef(project string, number *int) string {
	if number == nil {
		return project
	}
	return project + "#" + strconv.Itoa(*number)
}

// RunRef returns the operator-facing issue-scoped run reference.
func RunRef(project string, issueNumber int, runDisplay string) string {
	if issueNumber <= 0 {
		panic("run issue number must be positive")
	}
	runPart := strings.TrimSpace(runDisplay)
	if runPart == "" {
		runPart = "unknown"
	}
	return project + "#" + strconv.Itoa(issueNumber) + "/runs/" + runPart
}

// TouchpointRef returns the operator-facing touchpoint or pull request reference.
func TouchpointRef(repo string, number *int) string {
	if number == nil {
		return repo
	}
	return repo + "#" + strconv.Itoa(*number)
}

// LeaseRef returns the operator-facing lease reference.
func LeaseRef(project, slotName string, leaseNumber *int) string {
	if slotName != "" {
		return slotName
	}
	if leaseNumber != nil {
		return project + "/leases/" + strconv.Itoa(*leaseNumber)
	}
	return project + "/lease"
}
