package runnermcp

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"
)

type fakeUploader struct {
	blobName    string
	body        []byte
	contentType string
	retSize     int64
	retErr      error
	calls       int
}

func (f *fakeUploader) Upload(_ context.Context, blobName string, body []byte, contentType string) (int64, error) {
	f.calls++
	f.blobName = blobName
	f.body = body
	f.contentType = contentType
	if f.retErr != nil {
		return 0, f.retErr
	}
	if f.retSize != 0 {
		return f.retSize, nil
	}
	return int64(len(body)), nil
}

func runUpload(t *testing.T, ws fstest.MapFS, up ArtifactUploader, args map[string]any) (map[string]any, error) {
	t.Helper()
	tool := NewUploadEvidenceTool(RunContext{Project: "ambience", RunID: "168.3"}, ws, up)
	raw, _ := json.Marshal(args)
	res, err := tool.Handler(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", res)
	}
	return m, nil
}

func TestUploadEvidence_Video(t *testing.T) {
	ws := fstest.MapFS{"videos/dashboard.webm": {Data: []byte("webmbytes")}}
	up := &fakeUploader{}
	res, err := runUpload(t, ws, up, map[string]any{"path": "videos/dashboard.webm", "kind": "video"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if up.blobName != "runs/ambience/168.3/videos/dashboard.webm" {
		t.Fatalf("blobName = %q", up.blobName)
	}
	if up.contentType != "video/webm" {
		t.Fatalf("contentType = %q, want video/webm", up.contentType)
	}
	if res["ref"] != "blob://artifacts/runs/ambience/168.3/videos/dashboard.webm" {
		t.Fatalf("ref = %v", res["ref"])
	}
	if res["url"] != "/v1/artifacts/runs/ambience/168.3/videos/dashboard.webm" {
		t.Fatalf("url = %v", res["url"])
	}
	if res["size_bytes"].(int64) != int64(len("webmbytes")) {
		t.Fatalf("size_bytes = %v", res["size_bytes"])
	}
}

func TestUploadEvidence_ScreenshotInferredKind(t *testing.T) {
	ws := fstest.MapFS{"screenshots/final.png": {Data: []byte("png")}}
	up := &fakeUploader{}
	res, err := runUpload(t, ws, up, map[string]any{"path": "screenshots/final.png"}) // no kind
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if up.blobName != "runs/ambience/168.3/screenshots/final.png" {
		t.Fatalf("blobName = %q", up.blobName)
	}
	if up.contentType != "image/png" {
		t.Fatalf("contentType = %q", up.contentType)
	}
	if res["kind"] != "screenshot" {
		t.Fatalf("kind = %v, want screenshot", res["kind"])
	}
}

func TestUploadEvidence_ContentTypeOverride(t *testing.T) {
	ws := fstest.MapFS{"videos/x.bin": {Data: []byte("v")}}
	up := &fakeUploader{}
	_, err := runUpload(t, ws, up, map[string]any{"path": "videos/x.bin", "kind": "video", "content_type": "video/webm"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if up.contentType != "video/webm" {
		t.Fatalf("override ignored: contentType = %q", up.contentType)
	}
}

func TestUploadEvidence_Errors(t *testing.T) {
	ws := fstest.MapFS{"videos/run.webm": {Data: []byte("ok")}, "videos/empty.webm": {Data: []byte{}}}

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing file", map[string]any{"path": "videos/nope.webm", "kind": "video"}},
		{"path traversal", map[string]any{"path": "../secret", "kind": "video"}},
		{"empty path", map[string]any{"path": "", "kind": "video"}},
		{"empty file", map[string]any{"path": "videos/empty.webm", "kind": "video"}},
		{"unsupported kind", map[string]any{"path": "videos/run.webm", "kind": "trace"}},
		{"uninferable kind", map[string]any{"path": "notes.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeUploader{}
			if _, err := runUpload(t, ws, up, tc.args); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if up.calls != 0 {
				t.Fatalf("%s: upload must not be attempted on a rejected request", tc.name)
			}
		})
	}
}

func TestUploadEvidence_MissingRunContext(t *testing.T) {
	ws := fstest.MapFS{"videos/run.webm": {Data: []byte("ok")}}
	up := &fakeUploader{}
	tool := NewUploadEvidenceTool(RunContext{}, ws, up) // no project/run id
	raw, _ := json.Marshal(map[string]any{"path": "videos/run.webm", "kind": "video"})
	if _, err := tool.Handler(context.Background(), raw); err == nil {
		t.Fatal("expected error when run context is missing")
	}
	if up.calls != 0 {
		t.Fatal("upload must not happen without run context")
	}
}
