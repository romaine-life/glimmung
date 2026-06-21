// Package steperr holds the canonical wire shape for a typed step-body
// failure: the {layer, code, message} block a producer step may write to
// GLIMMUNG_COMPLETION_FILE so the runner can attribute the failure to a
// specific layer instead of a content-free "exited with code N".
//
// This type is the single source of truth shared by every honest producer
// surface: the public harness SDK (github.com/romaine-life/glimmung/harness/step)
// serializes its richer LayeredError down to this Block; the runner
// (cmd/glimmung-runner) reads it on step failure; the completion API
// (internal/server) threads it onto the run attempt; and the store
// (internal/store) promotes it into the terminal observation. Keeping the
// wire type in one domain package is what stops the SDK and the runner from
// drifting — there is exactly one definition to encode and decode.
package steperr

import "strings"

// Layer is the closed set of origins a typed step failure can be attributed
// to. It answers "whose fault is the crash" before any verification verdict
// exists:
//
//   - LayerHarness: the producer's own glue/harness code broke (a bug in the
//     SDK-driven step body, a missing required input, a panic). The work under
//     test never got a fair chance.
//   - LayerHost: the execution venue is unreachable or misconfigured — the
//     remote host could not be reached, an ssh-cert/authkey mint failed, a
//     tailnet peer was absent. Infrastructure, not the change under test.
//   - LayerModel: invoking the agent/model itself failed (the child agent CLI
//     crashed, exited non-zero, or never produced output). This reports only
//     whether the model RAN — never the verification verdict, which the
//     glimmung-owned verification_finalize primitive alone may write.
const (
	LayerHarness = "harness"
	LayerHost    = "host"
	LayerModel   = "model"
)

// Block is the typed step-error wire shape. It is intentionally minimal —
// {layer, code, message} — because it crosses a process boundary (written by
// the step body, read by the runner) and is persisted on the run. Richer
// producer-side context (subcommand, retryable, wrapped cause) lives on the
// SDK's LayeredError and is folded into Message before it becomes a Block.
type Block struct {
	// Layer is one of LayerHarness, LayerHost, or LayerModel. An empty or
	// unrecognized layer is treated as LayerHarness by Normalize, because an
	// untyped producer crash is, by definition, the harness failing to be
	// honest about its own failure.
	Layer string `json:"layer"`
	// Code is an optional stable, machine-readable short code for the failure
	// class (e.g. "missing_input", "agent_exec_failed", "host_unreachable").
	Code string `json:"code,omitempty"`
	// Message is the operator-facing reason. It must be non-empty for a
	// well-formed block; a Block with no message is not honest and Valid
	// rejects it.
	Message string `json:"message"`
}

// ValidLayer reports whether layer is one of the canonical closed-enum values.
func ValidLayer(layer string) bool {
	switch strings.TrimSpace(layer) {
	case LayerHarness, LayerHost, LayerModel:
		return true
	default:
		return false
	}
}

// Valid reports whether the block carries a non-empty message — the one thing
// that cannot be synthesized. The layer is not part of validity because
// Normalize always coerces an empty or unknown layer to LayerHarness (an
// untyped crash is the harness failing to attribute itself). The runner only
// promotes a valid block into the terminal observation; a block with no
// message is ignored so a producer can never launder a content-free failure as
// if it were attributed.
func (b *Block) Valid() bool {
	return b != nil && strings.TrimSpace(b.Message) != ""
}

// Normalize returns a copy with the layer coerced into the closed enum and the
// fields trimmed. An empty or unknown layer collapses to LayerHarness: an
// untyped step crash is the harness failing to attribute itself.
func (b Block) Normalize() Block {
	layer := strings.TrimSpace(b.Layer)
	if !ValidLayer(layer) {
		layer = LayerHarness
	}
	return Block{
		Layer:   layer,
		Code:    strings.TrimSpace(b.Code),
		Message: strings.TrimSpace(b.Message),
	}
}

// SuspectedCause maps a step-error layer onto the verification failure
// suspected_cause closed enum (code_bug | test_expectation_mismatch |
// environment_config | harness_flake) used by internal/domain/decision and the
// verification.json failure block.
//
// The mapping is a deliberate default, documented so producers know what an
// unclassified layered failure will be triaged as:
//
//   - LayerHarness -> harness_flake: the harness/glue broke; not the change.
//   - LayerHost    -> environment_config: the venue/host is the problem.
//   - LayerModel   -> code_bug: a model-layer fault surfaces against the
//     producer's own code path, so it defaults to code_bug for triage. A
//     producer with better knowledge should set suspected_cause explicitly.
func SuspectedCause(layer string) string {
	switch strings.TrimSpace(layer) {
	case LayerHost:
		return "environment_config"
	case LayerModel:
		return "code_bug"
	case LayerHarness:
		return "harness_flake"
	default:
		return "harness_flake"
	}
}
