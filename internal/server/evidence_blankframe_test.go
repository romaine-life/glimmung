package server

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/romaine-life/glimmung/internal/blankframe"
)

func solidFrame(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func contentFrame(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				uint8((x*31 + y*17) % 256),
				uint8((x*13 + y*57) % 256),
				uint8((x*101 + y*7) % 256),
				255,
			})
		}
	}
	return img
}

func TestFirstFrameBlankError(t *testing.T) {
	cfg := blankframe.DefaultConfig()

	err := firstFrameBlankError("videos/run.webm", solidFrame(64, 64, color.RGBA{255, 255, 255, 255}), cfg)
	if err == nil {
		t.Fatal("blank white frame should produce a ValidationError")
	}
	var ve ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %T (%v)", err, err)
	}

	if err := firstFrameBlankError("videos/run.webm", contentFrame(64, 64), cfg); err != nil {
		t.Fatalf("content frame should pass, got %v", err)
	}
}

func TestVideoEvidenceFirstFrameError(t *testing.T) {
	orig := firstFrameExtractor
	t.Cleanup(func() { firstFrameExtractor = orig })
	ctx := context.Background()

	// Confirmed blank → hard ValidationError.
	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		return solidFrame(48, 48, color.RGBA{255, 255, 255, 255}), nil
	}
	if err := videoEvidenceFirstFrameError(ctx, "videos/run.webm", []byte("payload")); err == nil {
		t.Fatal("blank first frame must be rejected")
	} else {
		var ve ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want ValidationError, got %T", err)
		}
	}

	// Real content → pass.
	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		return contentFrame(48, 48), nil
	}
	if err := videoEvidenceFirstFrameError(ctx, "videos/run.webm", []byte("payload")); err != nil {
		t.Fatalf("content first frame should pass, got %v", err)
	}

	// Extraction failure → non-blocking (only confirmed-blank fails the gate).
	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		return nil, errors.New("ffmpeg unavailable")
	}
	if err := videoEvidenceFirstFrameError(ctx, "videos/run.webm", []byte("payload")); err != nil {
		t.Fatalf("extraction failure must not block evidence, got %v", err)
	}

	// Empty payload → no extraction attempted, no error.
	called := false
	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		called = true
		return nil, nil
	}
	if err := videoEvidenceFirstFrameError(ctx, "videos/run.webm", nil); err != nil {
		t.Fatalf("empty payload should pass, got %v", err)
	}
	if called {
		t.Fatal("extractor should not be called for empty payload")
	}
}
