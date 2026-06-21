package verification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		v       Verification
		wantErr bool
	}{
		{"pass", Verification{Status: StatusPass}, false},
		{"fail", Verification{Status: StatusFail}, false},
		{"error", Verification{Status: StatusError}, false},
		{"abort-with-reason", Verification{Status: StatusAbort, AbortReason: "host asleep"}, false},
		{"abort-without-reason", Verification{Status: StatusAbort}, true},
		{"unknown", Verification{Status: "bogus"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.v.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestWriteFinalizableShapeAndDirs(t *testing.T) {
	dir := t.TempDir()
	v := Verification{
		Status:  StatusFail,
		Reasons: []string{"claimed_result_not_observed"},
		Failure: &Failure{
			Expected:       "vigor gained: 8",
			Observed:       "vigor gained: 88",
			Where:          "tooltip",
			SuspectedCause: CauseCodeBug,
			CauseDetail:    "off-by-ten",
		},
		EvidenceRefs:    []string{"runs/p/r/screenshots/a.png"},
		Evidence:        []EvidenceRef{{Kind: "screenshot", Ref: "runs/p/r/screenshots/a.png"}},
		EvidenceResults: []EvidenceResult{{Kind: "screenshot", Passed: false}},
		Notes:           "see tooltip",
	}
	artifacts, err := WriteFinalizable(dir, v)
	if err != nil {
		t.Fatalf("WriteFinalizable: %v", err)
	}
	for _, sub := range []string{"screenshots", "evidence"} {
		if _, err := os.Stat(filepath.Join(artifacts, sub)); err != nil {
			t.Fatalf("missing artifacts/%s: %v", sub, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(artifacts, "verification.json"))
	if err != nil {
		t.Fatalf("read verification.json: %v", err)
	}
	// Decode into a generic map and assert the exact keys the finalizer
	// (evidence_gate.go verificationFinalizeRunScript) reads.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"status", "reasons", "failure", "evidence_refs", "evidence", "evidence_results", "notes"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("verification.json missing finalizer key %q; got %v", key, doc)
		}
	}
	failure, ok := doc["failure"].(map[string]any)
	if !ok {
		t.Fatalf("failure block not an object: %T", doc["failure"])
	}
	for _, key := range []string{"expected", "observed", "where", "suspected_cause", "cause_detail"} {
		if _, ok := failure[key]; !ok {
			t.Fatalf("failure block missing key %q; got %v", key, failure)
		}
	}
	results, ok := doc["evidence_results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("evidence_results wrong shape: %v", doc["evidence_results"])
	}
	row := results[0].(map[string]any)
	if row["kind"] != "screenshot" || row["passed"] != false {
		t.Fatalf("evidence_results row = %v", row)
	}
}

func TestWriteFinalizableRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteFinalizable(dir, Verification{Status: StatusAbort}); err == nil {
		t.Fatal("abort without reason must be rejected before writing")
	}
	if _, err := os.Stat(filepath.Join(ArtifactsDir(dir), "verification.json")); err == nil {
		t.Fatal("a malformed verdict must not be written")
	}
}

func TestGatePassDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	proceed, err := Gate(dir, func() Verification { return Verification{Status: StatusPass} })
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if !proceed {
		t.Fatal("a passing gate must let the caller proceed to the agent")
	}
	if _, err := os.Stat(filepath.Join(ArtifactsDir(dir), "verification.json")); err == nil {
		t.Fatal("a passing gate must not write the verdict yet")
	}
}

func TestGateFailWritesVerdictAndStopsAgent(t *testing.T) {
	dir := t.TempDir()
	proceed, err := Gate(dir, func() Verification {
		return Verification{
			Status:  StatusFail,
			Reasons: []string{"unit tests red"},
			Failure: &Failure{SuspectedCause: CauseCodeBug, Observed: "3 of 50 failed"},
		}
	})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if proceed {
		t.Fatal("a failing gate must NOT proceed to the agent (no model tokens spent)")
	}
	raw, err := os.ReadFile(filepath.Join(ArtifactsDir(dir), "verification.json"))
	if err != nil {
		t.Fatalf("failing gate must write the verdict: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["status"] != StatusFail {
		t.Fatalf("gate verdict status = %v, want fail", doc["status"])
	}
}

func TestCauseForLayer(t *testing.T) {
	if got := CauseForLayer("host"); got != CauseEnvironmentConfig {
		t.Fatalf("host -> %q, want environment_config", got)
	}
	if got := CauseForLayer("harness"); got != CauseHarnessFlake {
		t.Fatalf("harness -> %q, want harness_flake", got)
	}
}
