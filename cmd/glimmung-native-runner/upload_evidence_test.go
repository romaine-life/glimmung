package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestEvidencePrefix(t *testing.T) {
	cases := []struct {
		name    string
		project string
		runRef  string
		want    string
	}{
		{name: "project and run ref", project: "spirelens", runRef: "spirelens#12/3.1", want: "spirelens/spirelens#12/3.1"},
		{name: "trims slashes", project: "/ambience/", runRef: "/4.2/", want: "ambience/4.2"},
		{name: "missing run ref", project: "ambience", runRef: "", want: "ambience"},
		{name: "missing both", project: "", runRef: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := uploadEvidenceConfig{Project: tc.project, RunRef: tc.runRef}
			if got := cfg.evidencePrefix(); got != tc.want {
				t.Fatalf("evidencePrefix()=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanEvidenceUploadsWalksNestedFilesUnderPrefix(t *testing.T) {
	dir := t.TempDir()
	// Nested tree: a top-level screenshot, a nested screenshot, a video, and a
	// JSON sidecar. The walk must include every regular file keyed by its path
	// relative to the evidence dir, under the run-scoped prefix, with POSIX
	// separators — regardless of host separator.
	writeEvidenceFile(t, dir, "frame-01.png", []byte("png-bytes-1"))
	writeEvidenceFile(t, dir, filepath.Join("screenshots", "frame-02.png"), []byte("png-bytes-2"))
	writeEvidenceFile(t, dir, filepath.Join("videos", "run.webm"), []byte("webm-bytes"))
	writeEvidenceFile(t, dir, filepath.Join("observations", "obs.json"), []byte(`{"ok":true}`))

	planned, err := planEvidenceUploads(dir, "spirelens/3.1")
	if err != nil {
		t.Fatalf("planEvidenceUploads: %v", err)
	}

	got := map[string]string{}
	for _, item := range planned {
		got[item.BlobName] = string(item.Body)
	}
	want := map[string]string{
		"spirelens/3.1/frame-01.png":             "png-bytes-1",
		"spirelens/3.1/screenshots/frame-02.png": "png-bytes-2",
		"spirelens/3.1/videos/run.webm":          "webm-bytes",
		"spirelens/3.1/observations/obs.json":    `{"ok":true}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planned (key->bytes)=%#v, want %#v", got, want)
	}

	// Deterministic order: blob names sorted ascending.
	names := make([]string, 0, len(planned))
	for _, item := range planned {
		names = append(names, item.BlobName)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("planned blob names are not sorted: %v", names)
	}

	// Content types are inferred from the extension.
	ctByName := map[string]string{}
	for _, item := range planned {
		ctByName[item.BlobName] = item.ContentType
	}
	if ct := ctByName["spirelens/3.1/frame-01.png"]; ct != "image/png" {
		t.Fatalf("png content type=%q, want image/png", ct)
	}
	if ct := ctByName["spirelens/3.1/observations/obs.json"]; ct != "application/json" {
		t.Fatalf("json content type=%q, want application/json", ct)
	}
}

func TestPlanEvidenceUploadsEmptyDirIsNoOp(t *testing.T) {
	dir := t.TempDir()
	planned, err := planEvidenceUploads(dir, "ambience/1.1")
	if err != nil {
		t.Fatalf("planEvidenceUploads on empty dir: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("planned=%#v, want empty (empty dir is a no-op)", planned)
	}
}

func TestPlanEvidenceUploadsAbsentDirIsNoOp(t *testing.T) {
	planned, err := planEvidenceUploads(filepath.Join(t.TempDir(), "does-not-exist"), "ambience/1.1")
	if err != nil {
		t.Fatalf("planEvidenceUploads on absent dir: %v", err)
	}
	if planned != nil {
		t.Fatalf("planned=%#v, want nil (absent dir is a no-op)", planned)
	}
}

func TestPlanEvidenceUploadsEmptyDirEnvIsNoOp(t *testing.T) {
	planned, err := planEvidenceUploads("", "ambience/1.1")
	if err != nil {
		t.Fatalf("planEvidenceUploads with empty dir path: %v", err)
	}
	if planned != nil {
		t.Fatalf("planned=%#v, want nil (empty GLIMMUNG_EVIDENCE_DIR is a no-op)", planned)
	}
}

type fakeEvidenceBlobClient struct {
	puts []plannedUpload
}

func (f *fakeEvidenceBlobClient) UploadBuffer(_ context.Context, _ string, blobName string, body []byte, contentType string) error {
	f.puts = append(f.puts, plannedUpload{BlobName: blobName, Body: append([]byte(nil), body...), ContentType: contentType})
	return nil
}

func TestUploadPlannedEvidencePutsEachObjectAndReturnsRefs(t *testing.T) {
	planned := []plannedUpload{
		{BlobName: "spirelens/3.1/a.png", Body: []byte("a"), ContentType: "image/png"},
		{BlobName: "spirelens/3.1/b.webm", Body: []byte("b"), ContentType: "video/webm"},
	}
	client := &fakeEvidenceBlobClient{}
	refs, err := uploadPlannedEvidence(context.Background(), client, "artifacts", planned)
	if err != nil {
		t.Fatalf("uploadPlannedEvidence: %v", err)
	}
	if len(client.puts) != 2 {
		t.Fatalf("puts=%d, want 2", len(client.puts))
	}
	wantRefs := []string{"blob://artifacts/spirelens/3.1/a.png", "blob://artifacts/spirelens/3.1/b.webm"}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("refs=%v, want %v", refs, wantRefs)
	}
}

func TestEmitEvidenceCompletionWritesArtifacts(t *testing.T) {
	completionFile := filepath.Join(t.TempDir(), "completion.json")
	t.Setenv("GLIMMUNG_COMPLETION_FILE", completionFile)

	refs := []string{
		"blob://artifacts/spirelens/3.1/frame.png",
		"blob://artifacts/spirelens/3.1/run.webm",
	}
	if err := emitEvidenceCompletion(refs); err != nil {
		t.Fatalf("emitEvidenceCompletion: %v", err)
	}
	raw, err := os.ReadFile(completionFile)
	if err != nil {
		t.Fatalf("read completion file: %v", err)
	}
	var meta completionMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if len(meta.Evidence) != 2 {
		t.Fatalf("evidence=%#v, want 2", meta.Evidence)
	}
	if meta.Evidence[0].Ref != refs[0] || meta.Evidence[0].Kind != "screenshot" || meta.Evidence[0].Label != "frame.png" {
		t.Fatalf("evidence[0]=%#v", meta.Evidence[0])
	}
	if meta.Evidence[1].Kind != "video" {
		t.Fatalf("evidence[1] kind=%q, want video", meta.Evidence[1].Kind)
	}
	if meta.ScreenshotsMarkdown == "" {
		t.Fatalf("expected non-empty screenshots markdown")
	}
}

func TestEmitEvidenceCompletionNoOpWithoutCompletionFile(t *testing.T) {
	t.Setenv("GLIMMUNG_COMPLETION_FILE", "")
	if err := emitEvidenceCompletion([]string{"blob://artifacts/x.png"}); err != nil {
		t.Fatalf("emitEvidenceCompletion no-op: %v", err)
	}
}

func TestRunUploadEvidenceNoOpWhenEvidenceDirUnset(t *testing.T) {
	t.Setenv("GLIMMUNG_EVIDENCE_DIR", "")
	// No storage env set: must still succeed because the empty dir short-circuits
	// before any client construction.
	if err := runUploadEvidence(context.Background(), nil); err != nil {
		t.Fatalf("runUploadEvidence no-op: %v", err)
	}
}

func TestRunUploadEvidenceNoOpWhenEvidenceDirEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GLIMMUNG_EVIDENCE_DIR", dir)
	t.Setenv("AGENT_SCREENSHOT_STORAGE_ACCOUNT", "")
	t.Setenv("AGENT_SCREENSHOT_CONTAINER", "")
	// An empty (but present) evidence dir must succeed as a no-op without
	// requiring storage env or touching Azure.
	if err := runUploadEvidence(context.Background(), nil); err != nil {
		t.Fatalf("runUploadEvidence empty dir: %v", err)
	}
}

func writeEvidenceFile(t *testing.T, dir, rel string, body []byte) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}
