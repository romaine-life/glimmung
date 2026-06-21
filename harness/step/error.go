package step

import (
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// LayeredError is the typed failure a handler returns to attribute a step
// crash to a specific layer. Returning one (instead of a bare error) is how
// attribution happens: the harness translates the Layer onto the persisted
// {layer, code, message} block (steperr.Block) and onto the verification
// suspected_cause enum, so a harness crash before the model ran can never be
// mislabelled as a model failure.
//
// The Layer is the load-bearing field. By construction the only place a
// LayeredError with Layer == steperr.LayerModel may originate is the
// harness/agent package's Invoke — a model-layer failure means the agent
// process itself failed to run, never a verification verdict. Harness glue
// code emits LayerHarness; remote-host venues emit LayerHost.
type LayeredError struct {
	// Layer is one of steperr.LayerHarness, LayerHost, or LayerModel.
	Layer string
	// Subcommand is an optional sub-operation that failed (e.g. "git clone",
	// "tailscale up", "claude --print"). It is folded into the wire message.
	Subcommand string
	// Code is an optional stable short code for the failure class.
	Code string
	// Message is the operator-facing reason.
	Message string
	// Retryable records whether the producer believes a fresh attempt could
	// succeed. It is advisory context for the operator/decision trail; it does
	// not itself change the decision engine's routing.
	Retryable bool
	// Cause is an optional wrapped error, surfaced via Unwrap and folded into
	// the wire message.
	Cause error
}

// Error implements error. It renders layer, subcommand, message, and wrapped
// cause into a single human line for stderr and logs.
func (e *LayeredError) Error() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	layer := strings.TrimSpace(e.Layer)
	if layer == "" {
		layer = steperr.LayerHarness
	}
	b.WriteString(layer)
	if sub := strings.TrimSpace(e.Subcommand); sub != "" {
		b.WriteString(" [")
		b.WriteString(sub)
		b.WriteString("]")
	}
	if code := strings.TrimSpace(e.Code); code != "" {
		b.WriteString(" (")
		b.WriteString(code)
		b.WriteString(")")
	}
	if msg := strings.TrimSpace(e.Message); msg != "" {
		b.WriteString(": ")
		b.WriteString(msg)
	}
	if e.Cause != nil {
		if cause := strings.TrimSpace(e.Cause.Error()); cause != "" {
			b.WriteString(": ")
			b.WriteString(cause)
		}
	}
	return b.String()
}

// Unwrap exposes the wrapped cause for errors.Is / errors.As.
func (e *LayeredError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Block folds the LayeredError into the persisted steperr.Block wire shape.
// The Layer is normalized into the closed enum; the Message carries the full
// rendered explanation (subcommand + cause included) so the operator reading
// the terminal observation sees the real reason, not a content-free code.
func (e *LayeredError) Block() steperr.Block {
	if e == nil {
		return steperr.Block{Layer: steperr.LayerHarness, Message: "nil layered error"}.Normalize()
	}
	return steperr.Block{
		Layer:   e.Layer,
		Code:    e.Code,
		Message: e.Error(),
	}.Normalize()
}

// SuspectedCause maps the error's layer onto the verification suspected_cause
// enum (see steperr.SuspectedCause).
func (e *LayeredError) SuspectedCause() string {
	if e == nil {
		return steperr.SuspectedCause(steperr.LayerHarness)
	}
	return steperr.SuspectedCause(e.Layer)
}

// HarnessError constructs a LayerHarness LayeredError — the harness/glue code
// itself failed (missing input, internal bug, recovered panic).
func HarnessError(code, message string, cause error) *LayeredError {
	return &LayeredError{Layer: steperr.LayerHarness, Code: code, Message: message, Cause: cause}
}

// HostError constructs a LayerHost LayeredError — the execution venue is
// unreachable or misconfigured.
func HostError(code, message string, cause error) *LayeredError {
	return &LayeredError{Layer: steperr.LayerHost, Code: code, Message: message, Cause: cause}
}

// ModelError constructs a LayerModel LayeredError — invoking the agent/model
// process failed. Reserved for harness/agent.Invoke; it reports only that the
// model did not run, never a verification verdict.
func ModelError(code, message string, cause error) *LayeredError {
	return &LayeredError{Layer: steperr.LayerModel, Code: code, Message: message, Cause: cause}
}

// asLayeredError coerces any handler error into a LayeredError. A nil error
// returns nil; an already-typed *LayeredError passes through; any other error
// is wrapped as a harness-layer failure, because an untyped crash is the
// harness failing to attribute itself honestly.
func asLayeredError(err error) *LayeredError {
	if err == nil {
		return nil
	}
	if le, ok := err.(*LayeredError); ok {
		return le
	}
	return &LayeredError{Layer: steperr.LayerHarness, Message: err.Error(), Cause: err}
}
