package server

import "encoding/json"

// hotSwapHistoryEntries parses a lease's durable test_slot_hot_swap_history
// metadata array back into typed entries (oldest first, newest last). The store
// persists entries as marshaled maps; this is the read-side inverse.
func hotSwapHistoryEntries(lease Lease) []TestSlotHotSwapHistoryEntry {
	raw, ok := lease.Metadata["test_slot_hot_swap_history"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]TestSlotHotSwapHistoryEntry, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blob, err := json.Marshal(m)
		if err != nil {
			continue
		}
		var entry TestSlotHotSwapHistoryEntry
		if err := json.Unmarshal(blob, &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// hotSwapEntryJobName reads the job handle an apply_hot_swap entry was recorded
// against (diagnostics.job_name); empty when absent.
func hotSwapEntryJobName(entry TestSlotHotSwapHistoryEntry) string {
	if entry.Diagnostics == nil {
		return ""
	}
	name, _ := entry.Diagnostics["job_name"].(string)
	return name
}

// latestHotSwapEntryForJob returns the most recent apply_hot_swap or
// deploy_to_image history entry recorded against job, and whether one exists.
// The initial "running" entry and the terminal entry share a job_name, so the
// latest is the authoritative status. Both slot-mutating operations record
// against this surface so the one status/poll route serves either.
func latestHotSwapEntryForJob(lease Lease, job string) (TestSlotHotSwapHistoryEntry, bool) {
	var latest TestSlotHotSwapHistoryEntry
	found := false
	for _, e := range hotSwapHistoryEntries(lease) {
		if (e.Operation != "apply_hot_swap" && e.Operation != "deploy_to_image") || hotSwapEntryJobName(e) != job {
			continue
		}
		latest = e
		found = true
	}
	return latest, found
}

// isTerminalHotSwapStatus reports whether a hot-swap entry status is terminal
// (the finalizer has recorded the outcome) versus the initial "running".
func isTerminalHotSwapStatus(status string) bool {
	switch status {
	case "persisted", "build_failed", "swap_failed", "timeout", "deployed", "deploy_failed":
		return true
	}
	return false
}

// hotSwapJobHasTerminalEntry reports whether the lease already carries a
// terminal apply_hot_swap entry for job — the finalizer's idempotency guard
// against duplicate apiserver events and post-restart re-lists.
func hotSwapJobHasTerminalEntry(lease Lease, job string) bool {
	entry, ok := latestHotSwapEntryForJob(lease, job)
	return ok && isTerminalHotSwapStatus(entry.Status)
}
