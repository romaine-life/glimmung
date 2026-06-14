package runnermcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type fakeRecorder struct {
	calls   int
	gotKind string
	gotArgs CaptureArgs
	body    []byte
	err     error
}

func (f *fakeRecorder) Record(_ context.Context, kind string, args CaptureArgs, outPath string) error {
	f.calls++
	f.gotKind = kind
	f.gotArgs = args
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(outPath, f.body, 0o600)
}

func runCapture(t *testing.T, tool Tool, args map[string]any) (map[string]any, error) {
	t.Helper()
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

func TestCaptureVideoTool(t *testing.T) {
	up := &fakeUploader{}
	rec := &fakeRecorder{body: []byte("webm-bytes")}
	tool := NewCaptureVideoTool(RunContext{Project: "ambience", RunID: "168.3"}, up, rec)

	out, err := runCapture(t, tool, map[string]any{
		"url": "https://ambience-slot-1.test/dashboard", "label": "Dashboard Tour", "wait_ms": 5000,
	})
	if err != nil {
		t.Fatalf("capture_video: %v", err)
	}

	if rec.calls != 1 || rec.gotKind != "video" {
		t.Fatalf("recorder calls=%d kind=%q, want 1/video", rec.calls, rec.gotKind)
	}
	// Defaults applied; explicit wait honored.
	if rec.gotArgs.WaitMs != 5000 || rec.gotArgs.Width != 1280 || rec.gotArgs.Height != 720 || !rec.gotArgs.FullPage {
		t.Fatalf("args defaults wrong: %+v", rec.gotArgs)
	}
	if up.blobName != "runs/ambience/168.3/videos/dashboard-tour.webm" {
		t.Fatalf("blob_name = %q", up.blobName)
	}
	if up.contentType != "video/webm" {
		t.Fatalf("content_type = %q", up.contentType)
	}
	if out["kind"] != "video" || out["blob_name"] != up.blobName || out["source_url"] != "https://ambience-slot-1.test/dashboard" {
		t.Fatalf("result shape wrong: %+v", out)
	}
}

func TestCaptureScreenshotTool(t *testing.T) {
	up := &fakeUploader{}
	rec := &fakeRecorder{body: []byte("png-bytes")}
	tool := NewCaptureScreenshotTool(RunContext{Project: "ambience", RunID: "168.3"}, up, rec)

	out, err := runCapture(t, tool, map[string]any{"url": "https://x.test/", "label": "home", "full_page": false})
	if err != nil {
		t.Fatalf("capture_screenshot: %v", err)
	}
	if rec.gotKind != "screenshot" || rec.gotArgs.FullPage {
		t.Fatalf("screenshot args wrong: kind=%q fullPage=%v", rec.gotKind, rec.gotArgs.FullPage)
	}
	if up.blobName != "runs/ambience/168.3/screenshots/home.png" || up.contentType != "image/png" {
		t.Fatalf("blob=%q content=%q", up.blobName, up.contentType)
	}
	if out["kind"] != "screenshot" {
		t.Fatalf("result kind = %v", out["kind"])
	}
}

func TestCaptureToolErrors(t *testing.T) {
	rc := RunContext{Project: "ambience", RunID: "168.3"}

	// Missing url.
	if _, err := NewCaptureVideoTool(rc, &fakeUploader{}, &fakeRecorder{}).Handler(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing url must error")
	}

	// Recorder failure surfaces; nothing is uploaded.
	up := &fakeUploader{}
	rec := &fakeRecorder{err: errors.New("connect refused")}
	if _, err := runCapture(t, NewCaptureVideoTool(rc, up, rec), map[string]any{"url": "https://x.test"}); err == nil {
		t.Fatal("recorder error must surface")
	}
	if up.calls != 0 {
		t.Fatalf("nothing must upload when recording fails, got %d", up.calls)
	}

	// Empty capture file is rejected (never uploaded as evidence).
	up2 := &fakeUploader{}
	rec2 := &fakeRecorder{body: nil}
	if _, err := runCapture(t, NewCaptureVideoTool(rc, up2, rec2), map[string]any{"url": "https://x.test"}); err == nil {
		t.Fatal("empty capture must error")
	}
	if up2.calls != 0 {
		t.Fatalf("empty capture must not upload, got %d", up2.calls)
	}
}

func TestCaptureFileName(t *testing.T) {
	cases := []struct{ label, url, ext, want string }{
		{"Dashboard Tour", "https://x", ".webm", "dashboard-tour.webm"},
		{"", "https://ambience-slot-1.test/a/b", ".png", "ambience-slot-1-test-a-b.png"},
		{"  ", "http://h/", ".webm", "h.webm"},
	}
	for _, c := range cases {
		if got := captureFileName(c.label, c.url, c.ext); got != c.want {
			t.Fatalf("captureFileName(%q,%q) = %q, want %q", c.label, c.url, got, c.want)
		}
	}
}

func TestNewNodeRecorderRequiresEndpoint(t *testing.T) {
	if _, err := NewNodeRecorder("  "); err == nil {
		t.Fatal("NewNodeRecorder must reject an empty endpoint so capture tools are not registered without a browser")
	}
	rec, err := NewNodeRecorder("ws://slot.test:3000")
	if err != nil {
		t.Fatalf("NewNodeRecorder: %v", err)
	}
	if rec == nil {
		t.Fatal("recorder is nil")
	}
}
