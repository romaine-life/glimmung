package server

import (
	"fmt"
	"strings"
)

const glimmungWorkContextBranchPrefix = "glimmung/"

func workContextBranch(run RunReplayData, metadata map[string]any) string {
	if id := strings.TrimSpace(stringValue(metadata["work_context_id"])); id != "" {
		if strings.Contains(id, "/") {
			return id
		}
		return glimmungWorkContextBranchPrefix + id
	}
	if branch := strings.TrimSpace(stringValue(metadata["work_context_branch"])); branch != "" {
		return branch
	}
	return issueRunBranch(run)
}

func issueRunBranch(run RunReplayData) string {
	display := strings.TrimSpace(stringValueFromPtr(run.RunDisplayNumber))
	if display == "" {
		return ""
	}
	return fmt.Sprintf("issue-%d-run-%s", run.IssueNumber, display)
}
