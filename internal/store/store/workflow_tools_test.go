package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/romaine-life/glimmung/internal/server"
)

// TestRunnerJobDocRoundTripsTools guards the bug that made the runner-MCP
// sidecar dead on arrival: the store's runnerJobDoc had no Tools field, so
// workflowDocFromRegister silently dropped job.Tools on the way to the jsonb
// column. With tools never persisted, the launcher's `len(job.Tools) > 0`
// sidecar gate could never fire, so no workflow ever got a capture sidecar.
func TestRunnerJobDocRoundTripsTools(t *testing.T) {
	spec := server.RunnerJobSpec{
		ID:      "v",
		Managed: true,
		Tools:   []string{"capture_video", "capture_screenshot", "upload_evidence"},
	}

	doc := runnerJobDocFromSpec(spec)
	if len(doc.Tools) != 3 {
		t.Fatalf("spec->doc dropped tools: %#v", doc.Tools)
	}

	// Persisted form is JSON in a jsonb column; the tag must round-trip.
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var doc2 runnerJobDoc
	if err := json.Unmarshal(payload, &doc2); err != nil {
		t.Fatal(err)
	}

	back := jobFromDoc(doc2)
	if !reflect.DeepEqual(back.Tools, spec.Tools) {
		t.Fatalf("tools lost on store round-trip: got %#v want %#v", back.Tools, spec.Tools)
	}
}
