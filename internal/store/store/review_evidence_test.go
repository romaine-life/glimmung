package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/romaine-life/glimmung/internal/server"
)

func TestReviewDocPreservesStructuredEvidence(t *testing.T) {
	doc := reviewDoc{
		ID:      "tp-1",
		Project: "proj",
		Repo:    "owner/repo",
		Number:  123,
		Title:   "review",
		State:   "ready",
		Evidence: []server.ReviewEvidence{{
			Kind:         "screenshot",
			Ref:          "blob://artifacts/runs/proj/run-1/screenshots/default.png",
			Label:        "default",
			URL:          "/v1/artifacts/runs/proj/run-1/screenshots/default.png",
			ArtifactPath: "runs/proj/run-1/screenshots/default.png",
		}},
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := reviewDocFromPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	row := reviewRowFromDoc(decoded, nil, nil, nil, nil, nil, nil, time.Now().UTC())
	if len(row.Evidence) != 1 {
		t.Fatalf("row evidence=%#v", row.Evidence)
	}
	if row.Evidence[0].Kind != "screenshot" || row.Evidence[0].ArtifactPath != "runs/proj/run-1/screenshots/default.png" {
		t.Fatalf("row evidence=%#v", row.Evidence[0])
	}
}
