package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/romaine-life/glimmung/internal/server"
)

// TestPreviewEnvDocRoundTrip pins the durable JSON shape: the full
// PreviewEnvironment — including the CLAIMED (live_build_id/pushed_at) and
// OBSERVED (observed_build_id/observed_at) fields the dashboard and verifier
// depend on — survives a marshal/unmarshal through the stored payload.
func TestPreviewEnvDocRoundTrip(t *testing.T) {
	pushedAt := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	observedAt := pushedAt.Add(5 * time.Second)
	env := server.PreviewEnvironment{
		Project:           "glimmung",
		Name:              "preview-glimmung-s1",
		LeaseRef:          "preview-glimmung-preview-glimmung-s1",
		SessionID:         "1217",
		AuthorizedSubject: "svc:preview:owner",
		Enabled:           true,
		State:             server.PreviewStateStale,
		URL:               "https://preview-glimmung-s1.glimmung.dev.romaine.life/",
		UpstreamURL:       "http://127.0.0.1:8000",
		BackendPrefixes:   []string{"/api", "/healthz"},
		ImageTag:          "app-mainfp",
		EdgeImage:         "acr.io/edge:edge-v1",
		LiveBuildID:       "build-B",
		PushedAt:          &pushedAt,
		ObservedBuildID:   "build-A",
		ObservedAt:        &observedAt,
		Detail:            "edge serving build build-A, expected build-B",
	}

	payload, err := json.Marshal(newPreviewEnvDoc(env))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var doc previewEnvDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := doc.PreviewEnvironment

	if doc.ID != server.PreviewEnvDocID(env.Project, env.Name) {
		t.Fatalf("doc id = %q", doc.ID)
	}
	if got.State != server.PreviewStateStale {
		t.Fatalf("state = %q", got.State)
	}
	if got.LiveBuildID != "build-B" || got.ObservedBuildID != "build-A" {
		t.Fatalf("claimed/observed builds not preserved: live=%q observed=%q", got.LiveBuildID, got.ObservedBuildID)
	}
	if got.PushedAt == nil || !got.PushedAt.Equal(pushedAt) {
		t.Fatalf("pushed_at not preserved: %v", got.PushedAt)
	}
	if got.ObservedAt == nil || !got.ObservedAt.Equal(observedAt) {
		t.Fatalf("observed_at not preserved: %v", got.ObservedAt)
	}
	if got.AuthorizedSubject != "svc:preview:owner" {
		t.Fatalf("authorized_subject not preserved: %q", got.AuthorizedSubject)
	}
	if len(got.BackendPrefixes) != 2 {
		t.Fatalf("backend_prefixes not preserved: %v", got.BackendPrefixes)
	}
}

// TestPreviewEnvETagRoundTrip pins the CAS etag format used by
// UpdatePreviewEnvironmentIfMatch.
func TestPreviewEnvETagRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 21, 12, 0, 0, 123456789, time.UTC)
	etag := previewEnvETagFromUpdatedAt(ts)
	parsed, err := parsePreviewEnvETag(etag)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Equal(ts) {
		t.Fatalf("etag round-trip: got %v want %v", parsed, ts)
	}
	if _, err := parsePreviewEnvETag(""); err == nil {
		t.Fatalf("empty etag must error")
	}
}
