package blankframe

import (
	"image"
	"image/color"
	"testing"
)

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// mostlyColorWithBlack fills a w*h frame with `bg`, then overwrites the first
// nBlack pixels (row-major) with black — a deterministic way to dial the
// content fraction.
func mostlyColorWithBlack(w, h int, bg color.RGBA, nBlack int) *image.RGBA {
	img := solid(w, h, bg)
	count := 0
	for y := 0; y < h && count < nBlack; y++ {
		for x := 0; x < w && count < nBlack; x++ {
			img.SetRGBA(x, y, color.RGBA{0, 0, 0, 255})
			count++
		}
	}
	return img
}

func gradientH(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		v := uint8(x * 255 / max(1, w-1))
		for y := 0; y < h; y++ {
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

// pseudoNoise is deterministic high-entropy content (no rand, reproducible).
func pseudoNoise(w, h int) *image.RGBA {
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

func halfHalf(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{0, 0, 0, 255})
			}
		}
	}
	return img
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var (
	white = color.RGBA{255, 255, 255, 255}
	black = color.RGBA{0, 0, 0, 255}
	gray  = color.RGBA{128, 128, 128, 255}
)

func TestAnalyze_Verdicts(t *testing.T) {
	// 100x100 = 10_000 pixels; DefaultConfig.MaxSamples (200k) inspects all of
	// them, so content fractions below are exact.
	cfg := DefaultConfig()
	cases := []struct {
		name      string
		img       image.Image
		wantBlank bool
	}{
		{"solid white", solid(100, 100, white), true},
		{"solid black", solid(100, 100, black), true},
		{"solid gray", solid(100, 100, gray), true},
		{"near-white 0.4% specks (40px)", mostlyColorWithBlack(100, 100, white, 40), true},
		{"white 1% content (100px)", mostlyColorWithBlack(100, 100, white, 100), false},
		{"white 2% content (200px)", mostlyColorWithBlack(100, 100, white, 200), false},
		{"horizontal gradient", gradientH(100, 100), false},
		{"pseudo noise", pseudoNoise(100, 100), false},
		{"half white / half black", halfHalf(100, 100), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.img, cfg)
			if got.Blank != tc.wantBlank {
				t.Fatalf("Blank = %v, want %v (uniformFraction=%.4f, sampled=%d, avg=%d,%d,%d)",
					got.Blank, tc.wantBlank, got.UniformFraction, got.Sampled, got.AvgR, got.AvgG, got.AvgB)
			}
		})
	}
}

func TestAnalyze_Deterministic(t *testing.T) {
	img := mostlyColorWithBlack(120, 80, white, 73)
	cfg := DefaultConfig()
	a := Analyze(img, cfg)
	b := Analyze(img, cfg)
	if a != b {
		t.Fatalf("non-deterministic: %+v != %+v", a, b)
	}
}

func TestAnalyze_ConfigTightening(t *testing.T) {
	// 50px of content out of 10_000 = 0.5% → uniformFraction 0.995, exactly at
	// the default boundary (blank). Tightening the bar to 0.999 flips it.
	img := mostlyColorWithBlack(100, 100, white, 50)
	if !IsBlank(img, DefaultConfig()) {
		t.Fatalf("expected blank at default 0.995 boundary")
	}
	strict := DefaultConfig()
	strict.UniformFraction = 0.999
	if IsBlank(img, strict) {
		t.Fatalf("expected not blank once UniformFraction tightened to 0.999")
	}
}

func TestAnalyze_NoiseAroundFlatColorStillBlank(t *testing.T) {
	// A flat white frame carrying mild per-pixel compression noise (±6) should
	// still read as blank: the average is white and every pixel sits within the
	// default epsilon of it.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			n := uint8(249 + (x+y)%7) // 249..255
			img.SetRGBA(x, y, color.RGBA{n, n, n, 255})
		}
	}
	if !IsBlank(img, DefaultConfig()) {
		t.Fatalf("flat near-white frame with mild noise should be blank")
	}
}

func TestAnalyze_Empty(t *testing.T) {
	got := Analyze(image.NewRGBA(image.Rect(0, 0, 0, 0)), DefaultConfig())
	if got.Blank {
		t.Fatalf("empty frame must not be reported blank")
	}
}

func TestAnalyze_SubsamplingStable(t *testing.T) {
	// A large solid frame must still read blank when subsampled by MaxSamples,
	// and a large content frame must still read not-blank.
	big := solid(2000, 2000, white)
	if !IsBlank(big, DefaultConfig()) {
		t.Fatalf("large solid frame should be blank under subsampling")
	}
	if IsBlank(pseudoNoise(2000, 2000), DefaultConfig()) {
		t.Fatalf("large noise frame should not be blank under subsampling")
	}
}
