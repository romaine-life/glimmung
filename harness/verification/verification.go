// Package verification provides typed helpers that FILL the existing
// Glimmung evidence frame — they never replace it. The verdict is still
// written only by the glimmung-owned verification_finalize step primitive
// (internal/server/evidence_gate.go); these helpers produce the
// artifacts/verification.json that finalizer reads, in the EXACT shape it
// expects, so a producer cannot emit a malformed verdict and cannot bypass
// the finalizer by emitting a repo-owned `verification` phase output.
package verification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// Status is the verifier verdict the finalizer accepts. "abort" requires a
// non-empty AbortReason and the finalizer maps it to a fail verdict carrying
// the reason.
const (
	StatusPass  = "pass"
	StatusFail  = "fail"
	StatusError = "error"
	StatusAbort = "abort"
)

// Suspected-cause closed enum, mirroring internal/domain/decision's
// VerificationFailure.SuspectedCause. A non-pass verdict should carry one.
const (
	CauseCodeBug                 = "code_bug"
	CauseTestExpectationMismatch = "test_expectation_mismatch"
	CauseEnvironmentConfig       = "environment_config"
	CauseHarnessFlake            = "harness_flake"
)

// Verification is the typed artifacts/verification.json document. Its JSON
// shape is exactly what internal/server/evidence_gate.go's
// verificationFinalizeRunScript reads (status / abort_reason / reasons /
// evidence_results / evidence_refs / evidence / notes / failure).
//
// The finalizer reads its known fields and PRESERVES every other top-level key.
// A consumer with a richer domain verdict (spirelens carries unit_tests /
// live_mcp_validation / screenshot_validation rows, ambience will carry its own)
// puts those extra keys in Extra: they round-trip through the SDK writer
// verbatim and feed the recycle context and human review intact, while the
// finalizer's known fields stay typed and validated here. This is what lets a
// consumer carry domain fields through the SDK writer WITHOUT forking it.
//
// A key owned by a typed field above (status, reasons, …) may not appear in
// Extra — Marshal rejects the collision so the typed field always wins and the
// document can never carry two conflicting copies of a finalizer-read key.
type Verification struct {
	Status          string           `json:"status"`
	Reasons         []string         `json:"reasons,omitempty"`
	Failure         *Failure         `json:"failure,omitempty"`
	AbortReason     string           `json:"abort_reason,omitempty"`
	Notes           string           `json:"notes,omitempty"`
	EvidenceRefs    []string         `json:"evidence_refs,omitempty"`
	Evidence        []EvidenceRef    `json:"evidence,omitempty"`
	EvidenceResults []EvidenceResult `json:"evidence_results,omitempty"`

	// Extra carries producer-domain top-level fields the finalizer preserves but
	// does not interpret. It is never emitted under a reserved key (see above).
	Extra map[string]json.RawMessage `json:"-"`
}

// reservedKeys are the top-level JSON keys owned by Verification's typed fields.
// Extra must not shadow any of them.
var reservedKeys = map[string]struct{}{
	"status":           {},
	"reasons":          {},
	"failure":          {},
	"abort_reason":     {},
	"notes":            {},
	"evidence_refs":    {},
	"evidence":         {},
	"evidence_results": {},
}

// MarshalJSON emits the typed known fields, then merges in Extra's
// producer-domain keys. A collision between Extra and a typed field is an error
// (the typed field is authoritative), so a producer cannot smuggle a second
// copy of a finalizer-read key past validation.
func (v Verification) MarshalJSON() ([]byte, error) {
	type known Verification // known has no MarshalJSON method, so no recursion.
	base, err := json.Marshal(known(v))
	if err != nil {
		return nil, err
	}
	if len(v.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, raw := range v.Extra {
		if _, reserved := reservedKeys[key]; reserved {
			return nil, fmt.Errorf("verification extra field %q collides with a typed field; set it on the typed field instead", key)
		}
		merged[key] = raw
	}
	return json.Marshal(merged)
}

// UnmarshalJSON decodes the typed known fields and captures every other
// top-level key into Extra, so a rich consumer document round-trips through the
// SDK without losing its domain fields.
func (v *Verification) UnmarshalJSON(data []byte) error {
	type known Verification
	var k known
	if err := json.Unmarshal(data, &k); err != nil {
		return err
	}
	*v = Verification(k)
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	for key := range reservedKeys {
		delete(all, key)
	}
	if len(all) > 0 {
		v.Extra = all
	}
	return nil
}

// Failure is the structured why of a non-pass verdict. Its JSON tags match
// internal/server's VerificationFailure / internal/store's
// verificationFailureDoc so it round-trips into the attempt record, abort
// explanations, and the next recycle cycle's prior-verification context.
type Failure struct {
	Expected       string `json:"expected,omitempty"`
	Observed       string `json:"observed,omitempty"`
	Where          string `json:"where,omitempty"`
	SuspectedCause string `json:"suspected_cause,omitempty"`
	CauseDetail    string `json:"cause_detail,omitempty"`
}

// EvidenceRef is a typed evidence artifact reference in verification.json.
type EvidenceRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// EvidenceResult is a per-evidence pass/fail row. The finalizer cross-checks a
// claimed-passed screenshot result against the presence of screenshot files,
// failing the verdict if a screenshot pass is claimed with no files present.
type EvidenceResult struct {
	Kind   string `json:"kind"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// CauseForLayer maps a step-error layer onto the suspected_cause enum so a
// producer turning a typed LayeredError into a verdict stays consistent with
// the rest of the SDK (see steperr.SuspectedCause).
func CauseForLayer(layer string) string { return steperr.SuspectedCause(layer) }

// Validate checks the verdict is well-formed before it is written, mirroring
// the finalizer's own rules so a malformed verdict is caught in-process rather
// than failing the gate at runtime.
func (v Verification) Validate() error {
	switch v.Status {
	case StatusPass, StatusFail, StatusError:
	case StatusAbort:
		if strings.TrimSpace(v.AbortReason) == "" {
			return fmt.Errorf("verification status=abort requires abort_reason")
		}
	default:
		return fmt.Errorf("invalid verification status %q (want pass, fail, error, or abort)", v.Status)
	}
	return nil
}

// ArtifactsDir returns the conventional artifacts directory for a working dir.
func ArtifactsDir(workingDir string) string {
	return filepath.Join(workingDir, "artifacts")
}

// WriteFinalizable validates v and lands it at
// ${workingDir}/artifacts/verification.json, creating the conventional
// artifacts/{screenshots,evidence} directories the finalizer scans. It returns
// the artifacts directory. This is the only sanctioned way for an SDK producer
// to feed the finalizer; the producer never writes the completion-file
// `verification` verdict itself.
func WriteFinalizable(workingDir string, v Verification) (string, error) {
	if strings.TrimSpace(workingDir) == "" {
		return "", fmt.Errorf("working dir required")
	}
	if err := v.Validate(); err != nil {
		return "", err
	}
	artifacts := ArtifactsDir(workingDir)
	for _, dir := range []string{artifacts, filepath.Join(artifacts, "screenshots"), filepath.Join(artifacts, "evidence")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode verification: %w", err)
	}
	path := filepath.Join(artifacts, "verification.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return artifacts, nil
}

// Gate runs a deterministic check before the expensive agent step. The check
// returns a Verification: a pass means the gate is clear and Gate returns
// proceed=true WITHOUT writing anything (the real verdict comes later from the
// agent + finalizer). A non-pass verdict is written via WriteFinalizable and
// Gate returns proceed=false — the caller MUST NOT invoke the agent, so a
// deterministic failure (e.g. unit tests already red) never spends model
// tokens.
func Gate(workingDir string, check func() Verification) (proceed bool, err error) {
	v := check()
	if v.Status == StatusPass {
		return true, nil
	}
	if _, err := WriteFinalizable(workingDir, v); err != nil {
		return false, err
	}
	return false, nil
}
