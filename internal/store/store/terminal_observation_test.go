package store

import (
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/decision"
	"github.com/romaine-life/glimmung/internal/server"
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

func TestTerminalObservationDoesNotNameZeroExitStep(t *testing.T) {
	exitCode := 0
	abortDecision := string(decision.AbortMalformed)
	doc := runDoc{
		Attempts: []attemptDoc{{
			AttemptIndex: 1,
			Phase:        "llm-work",
			Conclusion:   stringPtrValue("failure"),
			Decision:     &abortDecision,
			JobCompletions: map[string]nativeJobCompletionDoc{
				"llm-implement": {JobID: "llm-implement", Conclusion: "failure", TerminalReason: "job_failed"},
			},
		}},
		PhaseExecutions: []phaseExecutionDoc{{
			Name: "llm-work",
			Jobs: []jobExecutionDoc{{
				ID:     "llm-implement",
				State:  "failed",
				Reason: stringPtrValue("job_failed"),
				Steps: []stepExecutionDoc{{
					Slug:     "clone",
					State:    "failed",
					Reason:   stringPtrValue("job_failed"),
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
	if got.StepSlug != "" || got.ExitCode != nil {
		t.Fatalf("observation=%#v", got)
	}
	if !strings.Contains(got.Message, "producer phase llm-work failed at job llm-implement") || strings.Contains(got.Message, "clone") || strings.Contains(got.Message, "exit code 0") {
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

// Reproduces the run 13.1 cascade: a teardown phase (cleanup_early) whose pod
// failed to start (BackoffLimitExceeded -> timed_out) must never become the
// run's terminal cause, even if an abort decision was recorded on it. The
// terminal observation must attribute to the primary llm-verify failure, not
// the post-verdict cleanup blip that buried it.
func TestTerminalObservationSkipsFailedTeardownAttempt(t *testing.T) {
	verifyAbort := string(decision.AbortBudgetAttempts)
	teardownAbort := string(decision.AbortMalformed)
	doc := runDoc{
		Attempts: []attemptDoc{
			{
				AttemptIndex: 0,
				Phase:        "llm-verify",
				Conclusion:   stringPtrValue("failure"),
				Decision:     &verifyAbort,
				JobCompletions: map[string]nativeJobCompletionDoc{
					"llm-verify": {JobID: "llm-verify", Conclusion: "failure", TerminalReason: "verification_failed"},
				},
			},
			{
				AttemptIndex: 1,
				Phase:        "cleanup_early",
				Conclusion:   stringPtrValue("timed_out"),
				Decision:     &teardownAbort,
				JobCompletions: map[string]nativeJobCompletionDoc{
					"env-destroy": {JobID: "env-destroy", Conclusion: "timed_out", TerminalReason: "backoff_exceeded"},
				},
			},
		},
		PhaseExecutions: []phaseExecutionDoc{
			{Name: "llm-verify", State: "failed", Jobs: []jobExecutionDoc{{ID: "llm-verify", State: "failed"}}},
			{Name: "cleanup_early", State: "failed", Jobs: []jobExecutionDoc{{ID: "env-destroy", State: "failed"}}},
		},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{
		{Name: "llm-verify", Verify: true},
		{Name: "cleanup_early", Purpose: server.PhasePurposeTeardown},
	}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Phase != "llm-verify" || got.JobID != "llm-verify" {
		t.Fatalf("teardown failure masked the real verdict: observation=%#v", got)
	}
	if got.Class != server.TerminalObservationVerifierFailed {
		t.Fatalf("want verifier_failed class, got %q (%#v)", got.Class, got)
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

func TestTerminalObservationNamesDispatchFailureAsDispatchStep(t *testing.T) {
	abortDecision := string(decision.AbortMalformed)
	abortReason := `forward_dispatch_failed: native lease state is "released" for ambience-slot-3, want claimed`
	doc := runDoc{
		Attempts: []attemptDoc{{
			AttemptIndex: 1,
			Phase:        "llm-work",
			Conclusion:   stringPtrValue("success"),
			Decision:     &abortDecision,
		}},
		PhaseExecutions: []phaseExecutionDoc{
			{Name: "llm-work", State: "succeeded"},
			{
				Name:   "llm-verify",
				State:  "failed",
				Reason: stringPtrValue("dispatch_failed"),
				Jobs: []jobExecutionDoc{{
					ID:     "llm-verify",
					State:  "failed",
					Reason: stringPtrValue("dispatch_failed"),
					Steps: []stepExecutionDoc{{
						Slug:   "dispatch",
						State:  "failed",
						Reason: stringPtrValue("dispatch_failed"),
					}},
				}},
			},
		},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}, {Name: "llm-verify", Verify: true}}}

	got := terminalObservationForRun(doc, wf, "aborted", &abortReason, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Class != server.TerminalObservationDispatchFailed ||
		got.Phase != "llm-verify" ||
		got.JobID != "llm-verify" ||
		got.StepSlug != "dispatch" ||
		got.Reason != "dispatch_failed" {
		t.Fatalf("observation=%#v", got)
	}
	if !strings.Contains(got.Message, "phase llm-verify failed to dispatch job llm-verify") {
		t.Fatalf("message=%q", got.Message)
	}
}

func TestCanonicalExecutionFailureReasonRoundTripsDispatchReason(t *testing.T) {
	for _, reason := range []string{"dispatch_failed", "dispatch_timeout"} {
		if got := canonicalExecutionFailureReason(reason); got != reason {
			t.Fatalf("canonicalExecutionFailureReason(%q)=%q", reason, got)
		}
	}
}
