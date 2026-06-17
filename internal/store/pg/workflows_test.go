package pg

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTombstoneWorkflowPayloadPreservesRowAsUnusableHiddenWorkflow(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 34, 56, 789, time.UTC)
	raw := []byte(`{"kind":"workflow","project":"ambience","name":"sidecartest","metadata":{"owner":"ops","usable":true,"visible":true},"phases":[]}`)

	updated, err := tombstoneWorkflowPayload(raw, "operator@example.test", now)
	if err != nil {
		t.Fatalf("tombstoneWorkflowPayload: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(updated, &payload); err != nil {
		t.Fatalf("decode updated payload: %v", err)
	}
	if payload["name"] != "sidecartest" || payload["project"] != "ambience" {
		t.Fatalf("identity changed: %#v", payload)
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %#v", payload["metadata"])
	}
	if metadata["owner"] != "ops" {
		t.Fatalf("metadata owner=%#v, want preserved", metadata["owner"])
	}
	if metadata["deleted_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("deleted_at=%#v", metadata["deleted_at"])
	}
	if metadata["deleted_by"] != "operator@example.test" {
		t.Fatalf("deleted_by=%#v", metadata["deleted_by"])
	}
	if metadata["usable"] != false || metadata["visible"] != false {
		t.Fatalf("usable/visible=%#v/%#v, want false/false", metadata["usable"], metadata["visible"])
	}
}

func TestWorkflowDeleteDoesNotPhysicallyDeleteWorkflowRows(t *testing.T) {
	src, err := os.ReadFile("workflows.go")
	if err != nil {
		t.Fatalf("read workflows.go: %v", err)
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(string(src)), " "))
	if strings.Contains(normalized, "delete from workflows") {
		t.Fatal("workflow delete must tombstone the workflow row; physical DELETE FROM workflows strands historical runs")
	}
}
