package server

import "encoding/json"

// slotOpHistoryEntries parses a lease's durable test_slot_op_history metadata
// array back into typed entries (oldest first, newest last). The store persists
// entries as marshaled maps; this is the read-side inverse.
func slotOpHistoryEntries(lease Lease) []TestSlotOpHistoryEntry {
	raw, ok := lease.Metadata["test_slot_op_history"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]TestSlotOpHistoryEntry, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blob, err := json.Marshal(m)
		if err != nil {
			continue
		}
		var entry TestSlotOpHistoryEntry
		if err := json.Unmarshal(blob, &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// slotOpEntryJobName reads the job handle an operation entry was recorded
// against (diagnostics.job_name); empty when absent.
func slotOpEntryJobName(entry TestSlotOpHistoryEntry) string {
	if entry.Diagnostics == nil {
		return ""
	}
	name, _ := entry.Diagnostics["job_name"].(string)
	return name
}

// latestSlotOpEntryForJob returns the most recent image_deploy history entry
// recorded against job, and whether one exists. The initial "running" entry and
// the terminal entry share a job_name, so the latest is the authoritative status.
func latestSlotOpEntryForJob(lease Lease, job string) (TestSlotOpHistoryEntry, bool) {
	var latest TestSlotOpHistoryEntry
	found := false
	for _, e := range slotOpHistoryEntries(lease) {
		if e.Operation != "image_deploy" || slotOpEntryJobName(e) != job {
			continue
		}
		latest = e
		found = true
	}
	return latest, found
}

// isTerminalSlotOpStatus reports whether a slot operation entry status is
// terminal versus the initial "running".
func isTerminalSlotOpStatus(status string) bool {
	switch status {
	case "deployed", "deploy_failed":
		return true
	}
	return false
}
