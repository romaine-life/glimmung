package store

import (
	"strings"
	"testing"

	"github.com/nelsong6/glimmung/internal/domain/decision"
	"github.com/nelsong6/glimmung/internal/server"
)

func TestTerminalObservationNamesProducerJobAndStep(t *testing.T) {
	exitCode := 1
	abortDecision := string(decision.AbortMalformed)
	doc := runDoc{
		Attempts: []attemptDoc{
			{
				AttemptIndex: 1,
				Phase:        "llm-work",
				Conclusion:   stringPtrValue("failure"),
				Decision:     &abortDecision,
				JobCompletions: map[string]nativeJobCompletionDoc{
					"llm-test-plan": {JobID: "llm-test-plan", Conclusion: "success"},
					"llm-implement": {JobID: "llm-implement", Conclusion: "failure", TerminalReason: "job_failed"},
				},
			},
		},
		PhaseExecutions: []phaseExecutionDoc{{
			Name: "llm-work",
			Jobs: []jobExecutionDoc{{
				ID:     "llm-implement",
				State:  "failed",
				Reason: stringPtrValue("step_failed"),
				Steps: []stepExecutionDoc{{
					Slug:     "push-branch",
					State:    "failed",
					Reason:   stringPtrValue("exit_nonzero"),
					ExitCode: &exitCode,
				}},
			}},
		}},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Class != server.TerminalObservationProducerPhaseFailed ||
		got.Phase != "llm-work" ||
		got.JobID != "llm-implement" ||
		got.StepSlug != "push-branch" ||
		got.ExitCode == nil ||
		*got.ExitCode != 1 {
		t.Fatalf("observation=%#v", got)
	}
	if !strings.Contains(got.Message, "push-branch") || !strings.Contains(got.Message, "exit code 1") {
		t.Fatalf("message=%q", got.Message)
	}
}

func TestTerminalObservationKeepsOriginalAbortThroughCleanup(t *testing.T) {
	exitCode := 1
	abortDecision := string(decision.AbortMalformed)
	cleanupDecision := string(decision.Advance)
	doc := runDoc{
		Attempts: []attemptDoc{
			{
				AttemptIndex: 0,
				Phase:        "llm-work",
				Conclusion:   stringPtrValue("failure"),
				Decision:     &abortDecision,
				JobCompletions: map[string]nativeJobCompletionDoc{
					"llm-implement": {JobID: "llm-implement", Conclusion: "failure"},
				},
			},
			{
				AttemptIndex: 1,
				Phase:        "cleanup_final",
				Conclusion:   stringPtrValue("success"),
				Decision:     &cleanupDecision,
			},
		},
		PhaseExecutions: []phaseExecutionDoc{{
			Name: "llm-work",
			Jobs: []jobExecutionDoc{{
				ID: "llm-implement",
				Steps: []stepExecutionDoc{{
					Slug:     "push-branch",
					State:    "failed",
					ExitCode: &exitCode,
				}},
			}},
		}},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}, {Name: "cleanup_final", Purpose: server.PhasePurposeTeardown}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil || got.Phase != "llm-work" || got.JobID != "llm-implement" {
		t.Fatalf("observation=%#v", got)
	}
}

func TestTerminalObservationNamesVerifierContractMissing(t *testing.T) {
	abortDecision := string(decision.AbortMalformed)
	doc := runDoc{Attempts: []attemptDoc{{
		AttemptIndex: 0,
		Phase:        "llm-verify",
		Conclusion:   stringPtrValue("failure"),
		Decision:     &abortDecision,
	}}}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-verify", Verify: true}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Class != server.TerminalObservationVerifierContractMissing || got.Phase != "llm-verify" {
		t.Fatalf("observation=%#v", got)
	}
	if !strings.Contains(got.Message, "verification phase llm-verify") {
		t.Fatalf("message=%q", got.Message)
	}
}
