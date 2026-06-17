package server

import (
	"context"
	"time"
)

type TestSlotOpHistoryStore interface {
	AppendTestSlotOpHistory(ctx context.Context, project, leaseRef string, entry TestSlotOpHistoryEntry) (Lease, error)
}

type TestSlotOpHistoryEntry struct {
	Operation   string            `json:"operation"`
	Status      string            `json:"status"`
	Summary     string            `json:"summary,omitempty"`
	Diagnostics map[string]any    `json:"diagnostics,omitempty"`
	Timings     map[string]string `json:"timings,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}
