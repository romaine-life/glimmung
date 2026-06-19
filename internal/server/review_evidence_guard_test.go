package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRetiredReviewSurfaceReintroduced is a migration guard
// (docs/migration-policy.md): two retired surfaces must stay deleted in live
// server code.
//
//  1. The review gate's per-kind artifact COUNT — a divergent second evaluation
//     of the evidence contract that 422'd a run the verification phase had
//     already passed (spirelens#147: "required 2 screenshot evidence artifacts
//     but only 1 were recorded"). The gate now trusts the verify verdict and
//     only validates artifact durability; it must not re-derive
//     requiredEvidenceCounts / reviewArtifactRequirementKind.
//  2. The retired touchpoint / touchpoint_gate phase-primitive names. The live
//     primitives are pr_review / pr_merge.
//
// Behavior is separately pinned by
// TestFinalizeRunReviewByNumberAcceptsOneArtifactForMultipleScreenshotRequirements;
// this guard prevents the retired *surface* (symbols and error strings) from
// silently returning under any name's cover.
func TestNoRetiredReviewSurfaceReintroduced(t *testing.T) {
	retired := map[string]string{
		"requiredEvidenceCounts":        "review-gate per-kind evidence count (gate trusts the verify verdict, validates durability)",
		"reviewArtifactRequirementKind": "review-gate per-kind evidence count",
		"evidence artifacts but only":   "review-gate per-kind evidence count error string",
		"touchpoint_gate":               "retired touchpoint phase name (use review_gate)",
		"pr_touchpoint":                 "retired touchpoint primitive name (use pr_review)",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(src)
		for token, what := range retired {
			if strings.Contains(text, token) {
				t.Errorf("%s reintroduces retired surface %q (%s); see docs/migration-policy.md", filepath.Join("internal/server", name), token, what)
			}
		}
	}
}
