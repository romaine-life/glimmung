package store

import (
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/decision"
	"github.com/romaine-life/glimmung/internal/domain/steperr"
	"github.com/romaine-life/glimmung/internal/server"
)

// TestTerminalObservationPromotesTypedStepError is the §2 attribution test:
// when a producer step fails with a typed step-error block (and no verification
// verdict), the terminal observation carries the block's real message as the
// reason and the layer in the message — not the content-free "exit_nonzero".
func TestTerminalObservationPromotesTypedStepError(t *testing.T) {
	exitCode := 1
	abortDecision := string(decision.AbortMalformed)
	doc := runDoc{
		Attempts: []attemptDoc{{
			AttemptIndex: 1,
			Phase:        "env-prep",
			Conclusion:   stringPtrValue("failure"),
			Decision:     &abortDecision,
			JobCompletions: map[string]runnerJobCompletionDoc{
				"prepare-host": {
					JobID:      "prepare-host",
					Conclusion: "failure",
					Error: &steperr.Block{
						Layer:   steperr.LayerHost,
						Code:    "host_unreachable",
						Message: "warm host asleep: no tailnet host tagged tag:spirelens-host",
					},
				},
			},
		}},
		PhaseExecutions: []phaseExecutionDoc{{
			Name: "env-prep",
			Jobs: []jobExecutionDoc{{
				ID:     "prepare-host",
				State:  "failed",
				Reason: stringPtrValue("step_failed"),
				Steps: []stepExecutionDoc{{
					Slug:     "connect-host",
					State:    "failed",
					Reason:   stringPtrValue("exit_nonzero"),
					ExitCode: &exitCode,
				}},
			}},
		}},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "env-prep"}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Class != server.TerminalObservationProducerPhaseFailed {
		t.Fatalf("class=%q, want producer_phase_failed", got.Class)
	}
	if got.Reason != "warm host asleep: no tailnet host tagged tag:spirelens-host" {
		t.Fatalf("reason=%q, want the typed step-error message (not exit_nonzero)", got.Reason)
	}
	if got.Reason == "exit_nonzero" {
		t.Fatal("reason fell back to the generic exit_nonzero token")
	}
	if !strings.Contains(got.Message, "layer: host") {
		t.Fatalf("message=%q should carry the failing layer", got.Message)
	}
}

// TestTerminalObservationWithoutStepErrorIsUnchanged proves a producer failure
// with no typed block behaves exactly as before: reason falls back to the
// step execution reason.
func TestTerminalObservationWithoutStepErrorIsUnchanged(t *testing.T) {
	exitCode := 1
	abortDecision := string(decision.AbortMalformed)
	doc := runDoc{
		Attempts: []attemptDoc{{
			AttemptIndex: 1,
			Phase:        "env-prep",
			Conclusion:   stringPtrValue("failure"),
			Decision:     &abortDecision,
			JobCompletions: map[string]runnerJobCompletionDoc{
				"prepare-host": {JobID: "prepare-host", Conclusion: "failure"},
			},
		}},
		PhaseExecutions: []phaseExecutionDoc{{
			Name: "env-prep",
			Jobs: []jobExecutionDoc{{
				ID:     "prepare-host",
				State:  "failed",
				Reason: stringPtrValue("step_failed"),
				Steps: []stepExecutionDoc{{
					Slug:     "connect-host",
					State:    "failed",
					Reason:   stringPtrValue("exit_nonzero"),
					ExitCode: &exitCode,
				}},
			}},
		}},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "env-prep"}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Reason != "exit_nonzero" {
		t.Fatalf("reason=%q, want exit_nonzero (unchanged no-block behavior)", got.Reason)
	}
	if strings.Contains(got.Message, "layer:") {
		t.Fatalf("message=%q must not carry a step-error layer when no block was emitted", got.Message)
	}
}

// TestMalformedStepErrorIsDropped proves a block with no message (or unknown
// layer) cannot launder a hollow attribution: it is ignored and the reason
// falls back exactly as the no-block case.
func TestMalformedStepErrorIsDropped(t *testing.T) {
	if got := normalizedStepError(&steperr.Block{Layer: steperr.LayerHost}); got != nil {
		t.Fatalf("a block with no message must be dropped, got %+v", got)
	}
	if got := normalizedStepError(&steperr.Block{Message: "x", Layer: "bogus"}).Layer; got != steperr.LayerHarness {
		t.Fatalf("unknown layer should normalize to harness, got %q", got)
	}
}
