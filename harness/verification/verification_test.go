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

// TestExtraFieldsRoundTrip proves gap #5: a consumer's rich domain fields ride
// through WriteFinalizable verbatim while the finalizer's known fields stay
// typed, and the document round-trips back into typed-known + captured-extra.
func TestExtraFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	v := Verification{
		Status:          StatusFail,
		Reasons:         []string{"screenshot mismatch"},
		EvidenceResults: []EvidenceResult{{Kind: "screenshot", Passed: false}},
		Extra: map[string]json.RawMessage{
			"unit_tests":            json.RawMessage(`{"passed":true,"status":"green","failed":0}`),
			"live_mcp_validation":   json.RawMessage(`{"passed":true,"notes":"bridge ok"}`),
			"screenshot_validation": json.RawMessage(`{"passed":false,"count":2}`),
		},
	}
	artifacts, err := WriteFinalizable(dir, v)
	if err != nil {
		t.Fatalf("WriteFinalizable: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(artifacts, "verification.json"))
	if err != nil {
		t.Fatalf("read verification.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Known finalizer field stays typed and present.
	if doc["status"] != StatusFail {
		t.Fatalf("status = %v, want fail", doc["status"])
	}
	// Domain fields carried through verbatim.
	for _, key := range []string{"unit_tests", "live_mcp_validation", "screenshot_validation"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("extra field %q not carried into verification.json; got %v", key, doc)
		}
	}
	ut, ok := doc["unit_tests"].(map[string]any)
	if !ok || ut["status"] != "green" {
		t.Fatalf("unit_tests not preserved as an object: %v", doc["unit_tests"])
	}

	// Round-trip back: known fields decode typed, extras land in Extra.
	var back Verification
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("re-decode into Verification: %v", err)
	}
	if back.Status != StatusFail || len(back.EvidenceResults) != 1 || back.EvidenceResults[0].Kind != "screenshot" {
		t.Fatalf("known fields not typed on round-trip: %+v", back)
	}
	if _, ok := back.Extra["unit_tests"]; !ok {
		t.Fatalf("extra field not captured on round-trip: %+v", back.Extra)
	}
	// No reserved key leaks into Extra.
	for key := range reservedKeys {
		if _, ok := back.Extra[key]; ok {
			t.Fatalf("reserved key %q leaked into Extra", key)
		}
	}
}

// TestExtraReservedKeyCollisionRejected proves a producer cannot smuggle a
// second copy of a finalizer-read key through Extra.
func TestExtraReservedKeyCollisionRejected(t *testing.T) {
	dir := t.TempDir()
	v := Verification{
		Status: StatusPass,
		Extra:  map[string]json.RawMessage{"status": json.RawMessage(`"fail"`)},
	}
	if _, err := WriteFinalizable(dir, v); err == nil {
		t.Fatal("an Extra key colliding with a typed field must be rejected")
	}
	if _, err := os.Stat(filepath.Join(ArtifactsDir(dir), "verification.json")); err == nil {
		t.Fatal("a colliding verdict must not be written")
	}
}

// TestKnownFieldsValidatedDespiteExtra proves the typed contract still gates the
// write even when rich Extra fields are present.
func TestKnownFieldsValidatedDespiteExtra(t *testing.T) {
	dir := t.TempDir()
	v := Verification{
		Status: StatusAbort, // abort requires abort_reason — must fail validation
		Extra:  map[string]json.RawMessage{"unit_tests": json.RawMessage(`{"passed":true}`)},
	}
	if _, err := WriteFinalizable(dir, v); err == nil {
		t.Fatal("a malformed known field must be rejected regardless of Extra")
	}
	if _, err := os.Stat(filepath.Join(ArtifactsDir(dir), "verification.json")); err == nil {
		t.Fatal("a malformed verdict must not be written")
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
