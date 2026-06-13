// Package blankframe provides a deterministic, model-free test for whether a
// decoded frame is "blank": a near-uniform solid color such as the white
// about:blank browser shell that Playwright's recordVideo captures before the
// page paints. It exists to gate evidence videos whose first frame carries no
// real content.
//
// The verdict is pure pixel arithmetic — no model, no randomness — so the same
// frame and config always yield the same result. That determinism is what makes
// it safe to use as a hard evidence gate rather than an advisory hint.
package blankframe

import (
	"image"
	"math"
)

// Config holds the operator-tunable thresholds. These are control values, not
// magic constants: a deployment may tighten or loosen them. The defaults are
// calibrated for browser-capture frames and bias toward never flagging a frame
// that carries content as blank (a false positive would block good evidence).
type Config struct {
	// MaxSamples caps how many pixels are inspected. The frame is walked on a
	// deterministic square grid so the verdict is reproducible and largely
	// independent of resolution. Zero inspects every pixel.
	MaxSamples int

	// ChannelEpsilon is the per-channel (0-255) tolerance for treating a pixel
	// as the same color as the frame's average. A small epsilon absorbs mild
	// compression noise on an otherwise-flat frame.
	ChannelEpsilon int

	// UniformFraction is the share of sampled pixels that must fall within
	// ChannelEpsilon of the average color for the frame to be judged blank.
	// High by design: a real content frame has well under this share sitting on
	// a single color, so it is never falsely blanked.
	UniformFraction float64
}

// DefaultConfig returns the calibrated defaults: a frame is blank when at least
// 99.5% of sampled pixels sit within 8/255 per channel of the average color.
func DefaultConfig() Config {
	return Config{
		MaxSamples:      200_000,
		ChannelEpsilon:  8,
		UniformFraction: 0.995,
	}
}

// Result is the outcome of Analyze.
type Result struct {
	// Blank is the verdict.
	Blank bool

	// Sampled is how many pixels were inspected.
	Sampled int

	// UniformFraction is the share of sampled pixels within ChannelEpsilon of
	// the average color, in [0,1].
	UniformFraction float64

	// AvgR, AvgG, AvgB are the average color, 0-255 — useful for logging which
	// flat color a frame collapsed to (white about:blank vs a black GPU-warmup
	// frame, etc.).
	AvgR, AvgG, AvgB int
}

// Analyze samples img and reports whether it is a near-uniform solid color.
// It is deterministic: identical (img, cfg) always produce an identical Result.
// A frame with zero pixels is reported as not blank (Blank=false) rather than
// guessed, so a decode that yields nothing never blocks evidence on its own.
func Analyze(img image.Image, cfg Config) Result {
	if cfg.UniformFraction <= 0 {
		cfg.UniformFraction = DefaultConfig().UniformFraction
	}
	eps := cfg.ChannelEpsilon
	if eps < 0 {
		eps = 0
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return Result{}
	}

	stride := 1
	if cfg.MaxSamples > 0 {
		total := w * h
		if total > cfg.MaxSamples {
			stride = int(math.Ceil(math.Sqrt(float64(total) / float64(cfg.MaxSamples))))
			if stride < 1 {
				stride = 1
			}
		}
	}

	type px struct{ r, g, b int }
	samples := make([]px, 0, w*h/(stride*stride)+1)
	var sumR, sumG, sumB int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stride {
		for x := bounds.Min.X; x < bounds.Max.X; x += stride {
			r16, g16, b16, _ := img.At(x, y).RGBA()
			p := px{int(r16 >> 8), int(g16 >> 8), int(b16 >> 8)}
			samples = append(samples, p)
			sumR += p.r
			sumG += p.g
			sumB += p.b
		}
	}

	n := len(samples)
	if n == 0 {
		return Result{}
	}
	avgR := sumR / n
	avgG := sumG / n
	avgB := sumB / n

	within := 0
	for _, p := range samples {
		if abs(p.r-avgR) <= eps && abs(p.g-avgG) <= eps && abs(p.b-avgB) <= eps {
			within++
		}
	}

	frac := float64(within) / float64(n)
	return Result{
		Blank:           frac >= cfg.UniformFraction,
		Sampled:         n,
		UniformFraction: frac,
		AvgR:            avgR,
		AvgG:            avgG,
		AvgB:            avgB,
	}
}

// IsBlank is a convenience wrapper returning only the verdict.
func IsBlank(img image.Image, cfg Config) bool {
	return Analyze(img, cfg).Blank
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
