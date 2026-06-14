package runnermcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Tool names for the browser capture surface. They are the sanctioned way a run
// records web evidence; the per-repo capture scripts are retired.
const (
	ToolCaptureVideo      = "capture_video"
	ToolCaptureScreenshot = "capture_screenshot"
)

// captureScript is the Glimmung-owned browser capture helper, embedded into the
// binary so it travels with the tool and can never be a copyable per-repo
// script. The runner image ships node + Playwright; the recorder writes this to
// a temp file and execs it.
//
//go:embed capture.mjs
var captureScript []byte

const captureSchema = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "URL to capture. Use the leased test slot's URL."},
    "label": {"type": "string", "description": "Optional human-readable label; also names the stored file."},
    "wait_ms": {"type": "integer", "description": "Milliseconds to record/settle after the page is visible (default 3000)."},
    "width": {"type": "integer", "description": "Viewport width (default 1280)."},
    "height": {"type": "integer", "description": "Viewport height (default 720)."},
    "click": {"type": "string", "description": "Optional selector to click after the page loads."},
    "trigger_url": {"type": "string", "description": "Optional URL to POST after load (e.g. to fire an effect)."},
    "full_page": {"type": "boolean", "description": "Screenshot only: capture the full scrollable page (default true)."}
  },
  "required": ["url"]
}`

type captureKind struct {
	name        string
	dir         string
	ext         string
	contentType string
}

var (
	kindVideo      = captureKind{name: "video", dir: "videos", ext: ".webm", contentType: "video/webm"}
	kindScreenshot = captureKind{name: "screenshot", dir: "screenshots", ext: ".png", contentType: "image/png"}
)

// CaptureArgs is the normalized request the recorder receives.
type CaptureArgs struct {
	URL        string
	WaitMs     int
	Width      int
	Height     int
	Click      string
	TriggerURL string
	FullPage   bool
}

// BrowserRecorder records browser evidence against the leased slot browser and
// writes the artifact to outPath. The production implementation shells to the
// embedded Node screencast helper; tests inject a fake.
type BrowserRecorder interface {
	Record(ctx context.Context, kind string, args CaptureArgs, outPath string) error
}

// NewCaptureVideoTool records a video of a web page and uploads it as evidence.
// Recording starts after the page is visible, so the result never opens on a
// white about:blank frame.
func NewCaptureVideoTool(rc RunContext, up ArtifactUploader, rec BrowserRecorder) Tool {
	return captureTool(ToolCaptureVideo, kindVideo,
		"Record a video of a web page against the leased slot browser (recording starts after the page loads, never a blank frame) and upload it as evidence.",
		rc, up, rec)
}

// NewCaptureScreenshotTool captures a screenshot of a web page and uploads it as
// evidence.
func NewCaptureScreenshotTool(rc RunContext, up ArtifactUploader, rec BrowserRecorder) Tool {
	return captureTool(ToolCaptureScreenshot, kindScreenshot,
		"Capture a screenshot of a web page against the leased slot browser and upload it as evidence.",
		rc, up, rec)
}

func captureTool(name string, k captureKind, desc string, rc RunContext, up ArtifactUploader, rec BrowserRecorder) Tool {
	return Tool{
		Name:        name,
		Description: desc,
		InputSchema: json.RawMessage(captureSchema),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in struct {
				URL        string `json:"url"`
				Label      string `json:"label"`
				WaitMs     int    `json:"wait_ms"`
				Width      int    `json:"width"`
				Height     int    `json:"height"`
				Click      string `json:"click"`
				TriggerURL string `json:"trigger_url"`
				FullPage   *bool  `json:"full_page"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, fmt.Errorf("%s: invalid arguments: %w", name, err)
			}
			if strings.TrimSpace(in.URL) == "" {
				return nil, fmt.Errorf("%s: url is required", name)
			}
			if strings.TrimSpace(rc.Project) == "" || strings.TrimSpace(rc.RunID) == "" {
				return nil, fmt.Errorf("%s: run context is missing project/run id", name)
			}

			args := CaptureArgs{
				URL:        strings.TrimSpace(in.URL),
				WaitMs:     intOrDefault(in.WaitMs, 3000),
				Width:      intOrDefault(in.Width, 1280),
				Height:     intOrDefault(in.Height, 720),
				Click:      strings.TrimSpace(in.Click),
				TriggerURL: strings.TrimSpace(in.TriggerURL),
				FullPage:   in.FullPage == nil || *in.FullPage,
			}

			tmp, err := os.CreateTemp("", "glimmung-capture-*"+k.ext)
			if err != nil {
				return nil, fmt.Errorf("%s: create temp: %w", name, err)
			}
			outPath := tmp.Name()
			_ = tmp.Close()
			defer os.Remove(outPath)

			if err := rec.Record(ctx, k.name, args, outPath); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			body, err := os.ReadFile(outPath)
			if err != nil {
				return nil, fmt.Errorf("%s: read capture: %w", name, err)
			}
			if len(body) == 0 {
				return nil, fmt.Errorf("%s: capture produced an empty file", name)
			}

			blobName := path.Join("runs", rc.Project, rc.RunID, k.dir, captureFileName(in.Label, args.URL, k.ext))
			size, err := up.Upload(ctx, blobName, body, k.contentType)
			if err != nil {
				return nil, fmt.Errorf("%s: upload %q: %w", name, blobName, err)
			}

			return map[string]any{
				"ref":          "blob://artifacts/" + blobName,
				"url":          artifactURL(blobName),
				"blob_name":    blobName,
				"kind":         k.name,
				"label":        strings.TrimSpace(in.Label),
				"content_type": k.contentType,
				"size_bytes":   size,
				"source_url":   args.URL,
			}, nil
		},
	}
}

func intOrDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

var captureSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// captureFileName derives a stable, filesystem-safe blob file name from the
// label when given, else from the URL, so captures land at a predictable path.
func captureFileName(label, url, ext string) string {
	base := strings.TrimSpace(label)
	if base == "" {
		base = strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	}
	slug := strings.Trim(captureSlugRe.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if slug == "" {
		slug = "capture"
	}
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}
	return slug + ext
}

// nodeRecorder shells to the embedded Node helper to drive the slot browser.
type nodeRecorder struct {
	scriptPath string
	wsEndpoint string
}

// NewNodeRecorder writes the embedded capture helper to a temp file and returns
// a recorder bound to the leased slot browser's WebSocket endpoint. It returns
// an error when no endpoint is configured, so capture tools are not registered
// without a browser to drive.
func NewNodeRecorder(wsEndpoint string) (BrowserRecorder, error) {
	if strings.TrimSpace(wsEndpoint) == "" {
		return nil, fmt.Errorf("no slot browser ws endpoint configured")
	}
	f, err := os.CreateTemp("", "glimmung-capture-*.mjs")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(captureScript); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return &nodeRecorder{scriptPath: f.Name(), wsEndpoint: strings.TrimSpace(wsEndpoint)}, nil
}

func (r *nodeRecorder) Record(ctx context.Context, kind string, a CaptureArgs, outPath string) error {
	argv := []string{
		r.scriptPath,
		"--kind", kind,
		"--url", a.URL,
		"--output", outPath,
		"--wait-ms", strconv.Itoa(a.WaitMs),
		"--width", strconv.Itoa(a.Width),
		"--height", strconv.Itoa(a.Height),
	}
	if a.Click != "" {
		argv = append(argv, "--click", a.Click)
	}
	if a.TriggerURL != "" {
		argv = append(argv, "--trigger-url", a.TriggerURL)
	}
	if !a.FullPage {
		argv = append(argv, "--full-page", "false")
	}
	cmd := exec.CommandContext(ctx, "node", argv...)
	cmd.Env = append(os.Environ(), "PLAYWRIGHT_WS_ENDPOINT="+r.wsEndpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("capture helper failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
