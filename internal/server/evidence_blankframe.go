package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/romaine-life/glimmung/internal/blankframe"
)

// firstFrameExtractor is the seam used to pull a video's first frame. Production
// uses ffmpeg (present in the runtime image); tests inject a frame directly so
// the gate's verdict path is exercised without ffmpeg.
var firstFrameExtractor = ffmpegFirstFrame

const videoFirstFrameTimeout = 20 * time.Second

// videoEvidenceFirstFrameError returns a ValidationError when a video
// evidence artifact opens on a blank/near-uniform first frame — the unpainted
// browser shell (the about:blank white frame) that real evidence must not start
// on.
//
// Extraction failures are deliberately non-fatal: we reject only a *confirmed*
// blank frame, never an ffmpeg/codec hiccup, so the gate can't flake a run. A
// failure is surfaced via slog so a persistently-broken extractor shows up
// rather than silently disabling the gate.
func videoEvidenceFirstFrameError(ctx context.Context, blobName string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	frame, err := firstFrameExtractor(ctx, data)
	if err != nil {
		slog.WarnContext(ctx, "blank-frame gate could not decode evidence video first frame; not blocking",
			"artifact", blobName, "error", err)
		return nil
	}
	return firstFrameBlankError(blobName, frame, blankframe.DefaultConfig())
}

// firstFrameBlankError is the pure verdict→error mapping, separated from
// extraction so it is unit tested with a synthetic frame (no ffmpeg needed).
func firstFrameBlankError(blobName string, frame image.Image, cfg blankframe.Config) error {
	res := blankframe.Analyze(frame, cfg)
	if !res.Blank {
		return nil
	}
	return ValidationError{Message: fmt.Sprintf(
		"video evidence %s opens on a blank first frame (near-uniform color %d,%d,%d across %.1f%% of pixels); evidence must start on painted content",
		blobName, res.AvgR, res.AvgG, res.AvgB, res.UniformFraction*100,
	)}
}

// ffmpegFirstFrame extracts frame 0 of a video as PNG via ffmpeg (in the runtime
// image) and decodes it. ffmpeg reads from a temp file so it can seek the
// container; the PNG is captured from stdout.
func ffmpegFirstFrame(ctx context.Context, data []byte) (image.Image, error) {
	tmp, err := os.CreateTemp("", "glimmung-evidence-*.video")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, videoFirstFrameTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", tmp.Name(),
		"-frames:v", "1",
		"-f", "image2pipe", "-vcodec", "png",
		"pipe:1",
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg first-frame extract: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	img, err := png.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("decode first-frame png: %w", err)
	}
	return img, nil
}
