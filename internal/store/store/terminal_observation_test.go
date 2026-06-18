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
				JobCompletions: map[string]runnerJobCompletionDoc{
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
			JobCompletions: map[string]runnerJobCompletionDoc{
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
				JobCompletions: map[string]runnerJobCompletionDoc{
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
				JobCompletions: map[string]runnerJobCompletionDoc{
					"llm-verify": {JobID: "llm-verify", Conclusion: "failure", TerminalReason: "verification_failed"},
				},
			},
			{
				AttemptIndex: 1,
				Phase:        "cleanup_early",
				Conclusion:   stringPtrValue("timed_out"),
				Decision:     &teardownAbort,
				JobCompletions: map[string]runnerJobCompletionDoc{
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
	abortReason := `forward_dispatch_failed: runner lease state is "released" for ambience-slot-3, want claimed`
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

// TestTerminalObservationAttributesEveryClass is the contract test for the
// platform invariant: no run reaches a terminal failure state without a typed
// terminal observation that names a known non-success class, carries owner
// identity (phase + job + step_slug where applicable), and a SPECIFIC
// reason/message — for EVERY terminal failure class in the enum.
// terminalObservationClassCase is one per-class attribution fixture, extracted
// to package scope so both the attribution contract test and the canonical-list
// connection test (TestTerminalObservationFixtureCoversEveryCanonicalClass) can
// iterate the SAME fixtures without duplicating them.
type terminalObservationClassCase struct {
	name             string
	doc              runDoc
	wf               *server.Workflow
	abortReason      *string
	wantClass        string
	wantPhase        string
	wantJob          string
	wantStep         string
	wantReason       string
	msgContains      []string
	msgNotContains   []string
	ownerlessAllowed bool // manual_abort has no phase/job owner
}

// terminalObservationClassFixtures returns the attribution fixture for every
// terminal failure class. Each fixture drives terminalObservationForRun and
// asserts the produced observation names a known class with owner identity and
// a specific (non-empty) message.
func terminalObservationClassFixtures() []terminalObservationClassCase {
	exit1 := 1

	producerAbort := string(decision.AbortMalformed)
	verifyAbort := string(decision.AbortBudgetAttempts)
	gateAbort := string(decision.AbortMalformed)
	requestedAbort := string(decision.AbortRequested)
	malformedAbort := string(decision.AbortMalformed)

	verifierVerification := &verificationDoc{
		Status:  "fail",
		Reasons: []string{"claimed_result_not_observed"},
		Failure: &verificationFailureDoc{
			Expected:       `on-screen text "CLOAK_CLASP"`,
			Observed:       `display name "Cloak Clasp"`,
			Where:          "decoded frame",
			SuspectedCause: "test_expectation_mismatch",
			CauseDetail:    "the game UI renders the display name, not the literal token",
		},
	}

	cases := []terminalObservationClassCase{
		{
			name: "producer_phase_failed",
			doc: runDoc{
				Attempts: []attemptDoc{{
					AttemptIndex: 0,
					Phase:        "llm-work",
					Conclusion:   stringPtrValue("failure"),
					Decision:     &producerAbort,
					JobCompletions: map[string]runnerJobCompletionDoc{
						"llm-implement": {JobID: "llm-implement", Conclusion: "failure", TerminalReason: "job_failed"},
					},
				}},
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
							ExitCode: &exit1,
						}},
					}},
				}},
			},
			wf:          &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}}},
			wantClass:   server.TerminalObservationProducerPhaseFailed,
			wantPhase:   "llm-work",
			wantJob:     "llm-implement",
			wantStep:    "push-branch",
			msgContains: []string{"producer phase llm-work", "push-branch", "exit code 1"},
		},
		{
			name: "verifier_failed",
			doc: runDoc{
				Attempts: []attemptDoc{{
					AttemptIndex: 0,
					Phase:        "llm-verify",
					Conclusion:   stringPtrValue("failure"),
					Decision:     &verifyAbort,
					Verification: verifierVerification,
					JobCompletions: map[string]runnerJobCompletionDoc{
						"llm-verify": {
							JobID:          "llm-verify",
							Conclusion:     "failure",
							TerminalReason: "verification_failed",
							Verification:   verifierVerification,
						},
					},
				}},
				PhaseExecutions: []phaseExecutionDoc{{
					Name:  "llm-verify",
					State: "failed",
					Jobs: []jobExecutionDoc{{
						ID:    "llm-verify",
						State: "failed",
						// Every step exited 0 — the spirelens#147 shape.
						Steps: []stepExecutionDoc{{Slug: "judge-evidence", State: "succeeded"}},
					}},
				}},
			},
			wf:             &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-verify", Verify: true}}},
			wantClass:      server.TerminalObservationVerifierFailed,
			wantPhase:      "llm-verify",
			wantJob:        "llm-verify",
			wantReason:     "claimed_result_not_observed",
			msgContains:    []string{"claimed_result_not_observed", "expected:", "observed:", "decoded frame", "test_expectation_mismatch"},
			msgNotContains: []string{": verification_failed"},
		},
		{
			name: "gate_failed",
			doc: runDoc{
				Attempts: []attemptDoc{{
					AttemptIndex: 0,
					Phase:        "evidence-gate",
					Conclusion:   stringPtrValue("failure"),
					Decision:     &gateAbort,
					JobCompletions: map[string]runnerJobCompletionDoc{
						"evidence-gate": {
							JobID:        "evidence-gate",
							Conclusion:   "failure",
							Verification: &verificationDoc{Status: "fail", Reasons: []string{"required_evidence_absent"}},
						},
					},
				}},
				PhaseExecutions: []phaseExecutionDoc{{
					Name:  "evidence-gate",
					State: "failed",
					Jobs:  []jobExecutionDoc{{ID: "evidence-gate", State: "failed"}},
				}},
			},
			wf:          &server.Workflow{Phases: []server.PhaseSpec{{Name: "evidence-gate", EvidenceVerificationGate: true}}},
			wantClass:   server.TerminalObservationGateFailed,
			wantPhase:   "evidence-gate",
			wantJob:     "evidence-gate",
			wantReason:  "required_evidence_absent",
			msgContains: []string{"evidence gate", "required_evidence_absent"},
		},
		{
			name: "verifier_contract_missing",
			doc: runDoc{Attempts: []attemptDoc{{
				AttemptIndex: 0,
				Phase:        "llm-verify",
				Conclusion:   stringPtrValue("failure"),
				Decision:     &malformedAbort,
			}}},
			wf:          &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-verify", Verify: true}}},
			wantClass:   server.TerminalObservationVerifierContractMissing,
			wantPhase:   "llm-verify",
			msgContains: []string{"verification phase llm-verify"},
		},
		{
			name: "dispatch_failed",
			doc: runDoc{
				Attempts: []attemptDoc{{
					AttemptIndex: 1,
					Phase:        "llm-work",
					Conclusion:   stringPtrValue("success"),
					Decision:     &malformedAbort,
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
							Steps:  []stepExecutionDoc{{Slug: "dispatch", State: "failed", Reason: stringPtrValue("dispatch_failed")}},
						}},
					},
				},
			},
			wf:          &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}, {Name: "llm-verify", Verify: true}}},
			abortReason: stringPtrValue(`forward_dispatch_failed: runner lease state is "released" for ambience-slot-3`),
			wantClass:   server.TerminalObservationDispatchFailed,
			wantPhase:   "llm-verify",
			wantJob:     "llm-verify",
			wantStep:    "dispatch",
			wantReason:  "dispatch_failed",
			msgContains: []string{"phase llm-verify failed to dispatch job llm-verify"},
		},
		{
			name: "phase_requested_abort",
			doc: runDoc{Attempts: []attemptDoc{{
				AttemptIndex: 0,
				Phase:        "llm-work",
				Conclusion:   stringPtrValue("aborted"),
				Decision:     &requestedAbort,
				PhaseOutputs: map[string]string{decision.AbortReasonOutputKey: "unexpected_mod:godotexplorer"},
			}}},
			wf:          &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}}},
			wantClass:   server.TerminalObservationPhaseRequestedAbort,
			wantPhase:   "llm-work",
			wantReason:  "unexpected_mod:godotexplorer",
			msgContains: []string{"requested a fail-closed abort", "unexpected_mod:godotexplorer"},
		},
		{
			name:             "manual_abort",
			doc:              runDoc{},
			wf:               nil,
			abortReason:      stringPtrValue("operator stopped the run"),
			wantClass:        server.TerminalObservationManualAbort,
			wantReason:       "operator stopped the run",
			msgContains:      []string{"operator stopped the run"},
			ownerlessAllowed: true,
		},
		{
			name: "malformed_terminal",
			doc: runDoc{
				Attempts: []attemptDoc{{
					AttemptIndex: 0,
					Phase:        "llm-work",
					Conclusion:   stringPtrValue("failure"),
					Decision:     &malformedAbort,
					// No failed job completion, no abort_reason output, no
					// verification, no dispatch failure: unattributable.
				}},
			},
			wf:          &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}}},
			wantClass:   server.TerminalObservationMalformed,
			wantPhase:   "llm-work",
			msgContains: []string{"MALFORMED TERMINAL", "llm-work", "no job completions"},
		},
	}

	return cases
}

func TestTerminalObservationAttributesEveryClass(t *testing.T) {
	cases := terminalObservationClassFixtures()

	// knownClasses is derived from the canonical source-of-truth list rather
	// than a hand-maintained map, so the attribution contract test and the
	// regression guard can never disagree about the full set of terminal
	// classes.
	knownClasses := map[string]bool{}
	for _, class := range server.AllTerminalObservationClasses {
		knownClasses[class] = true
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := terminalObservationForRun(tc.doc, tc.wf, "aborted", tc.abortReason, server.TerminalObservationSourceCompletionCallback)
			if got == nil {
				t.Fatal("terminal observation missing — a terminal failure must always be attributed")
			}
			// Known, non-empty class.
			if got.Class == "" || got.Class == "unknown" || !knownClasses[got.Class] {
				t.Fatalf("class %q is not a known non-success class: %#v", got.Class, got)
			}
			if got.Class != tc.wantClass {
				t.Fatalf("class=%q want %q (%#v)", got.Class, tc.wantClass, got)
			}
			// Non-empty, specific message.
			if strings.TrimSpace(got.Message) == "" {
				t.Fatalf("message is empty: %#v", got)
			}
			// Owner identity where applicable.
			if !tc.ownerlessAllowed && got.Phase == "" {
				t.Fatalf("phase identity missing: %#v", got)
			}
			if tc.wantPhase != "" && got.Phase != tc.wantPhase {
				t.Fatalf("phase=%q want %q", got.Phase, tc.wantPhase)
			}
			if tc.wantJob != "" && got.JobID != tc.wantJob {
				t.Fatalf("job=%q want %q", got.JobID, tc.wantJob)
			}
			if tc.wantStep != "" && got.StepSlug != tc.wantStep {
				t.Fatalf("step_slug=%q want %q", got.StepSlug, tc.wantStep)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Fatalf("reason=%q want %q (%#v)", got.Reason, tc.wantReason, got)
			}
			for _, want := range tc.msgContains {
				if !strings.Contains(got.Message, want) {
					t.Fatalf("message %q missing %q", got.Message, want)
				}
			}
			for _, notWant := range tc.msgNotContains {
				if strings.Contains(got.Message, notWant) {
					t.Fatalf("message %q must not contain %q", got.Message, notWant)
				}
			}
		})
	}
}

// TestTerminalObservationFixtureCoversEveryCanonicalClass is the regression
// guard that CONNECTS slice 1's attribution coverage to the canonical
// source-of-truth list. It iterates server.AllTerminalObservationClasses and
// fails if any canonical class has no attribution fixture in
// terminalObservationClassFixtures — so a future class added to the list but
// left unattributed fails CI instead of being silently skipped. It also rejects
// the reverse drift: a fixture whose wantClass is not a canonical class.
func TestTerminalObservationFixtureCoversEveryCanonicalClass(t *testing.T) {
	fixtures := terminalObservationClassFixtures()

	fixtureClasses := map[string]int{}
	for _, fixture := range fixtures {
		if fixture.wantClass == "" {
			t.Fatalf("attribution fixture %q has an empty wantClass", fixture.name)
		}
		fixtureClasses[fixture.wantClass]++
	}

	canonical := map[string]bool{}
	for _, class := range server.AllTerminalObservationClasses {
		canonical[class] = true
		count := fixtureClasses[class]
		if count == 0 {
			t.Fatalf("canonical terminal class %q has NO attribution fixture — every class in AllTerminalObservationClasses must be covered by terminalObservationClassFixtures so it cannot settle unattributed", class)
		}
		if count > 1 {
			t.Fatalf("canonical terminal class %q has %d attribution fixtures — expected exactly one", class, count)
		}
	}

	for class := range fixtureClasses {
		if !canonical[class] {
			t.Fatalf("attribution fixture targets class %q which is not in AllTerminalObservationClasses — add it to the canonical list or remove the stale fixture", class)
		}
	}
}

// TestTerminalObservationVerifierCarriesReasonNotEnum is the focused regression
// for spirelens#147 run 1.1: the verify JOB went red while every STEP exited 0,
// and the specific reason lived only in the attempt verification. The terminal
// observation must carry the verifier's reason string, never the bare
// "verification_failed" enum.
func TestTerminalObservationVerifierCarriesReasonNotEnum(t *testing.T) {
	verifyAbort := string(decision.AbortBudgetAttempts)
	verification := &verificationDoc{
		Status:  "fail",
		Reasons: []string{"claimed_result_not_observed"},
		Failure: &verificationFailureDoc{
			Expected:       `on-screen text "CLOAK_CLASP"`,
			Observed:       `display name "Cloak Clasp"`,
			Where:          "decoded frame",
			SuspectedCause: "test_expectation_mismatch",
		},
	}
	doc := runDoc{
		Attempts: []attemptDoc{{
			AttemptIndex: 0,
			Phase:        "llm-verify",
			Conclusion:   stringPtrValue("failure"),
			Decision:     &verifyAbort,
			Verification: verification,
			JobCompletions: map[string]runnerJobCompletionDoc{
				"llm-verify": {JobID: "llm-verify", Conclusion: "failure", TerminalReason: "verification_failed", Verification: verification},
			},
		}},
		PhaseExecutions: []phaseExecutionDoc{{
			Name:  "llm-verify",
			State: "failed",
			Jobs:  []jobExecutionDoc{{ID: "llm-verify", State: "failed", Steps: []stepExecutionDoc{{Slug: "judge-evidence", State: "succeeded"}}}},
		}},
	}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-verify", Verify: true}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceCompletionCallback)
	if got == nil {
		t.Fatal("terminal observation missing")
	}
	if got.Class != server.TerminalObservationVerifierFailed {
		t.Fatalf("class=%q want verifier_failed", got.Class)
	}
	if got.Reason != "claimed_result_not_observed" {
		t.Fatalf("reason=%q want the verifier reason, not the bare enum", got.Reason)
	}
	if !strings.Contains(got.Message, "claimed_result_not_observed") {
		t.Fatalf("message %q must carry the verifier reason", got.Message)
	}
	if strings.Contains(got.Message, ": verification_failed") {
		t.Fatalf("message %q must not fall back to the bare enum", got.Message)
	}
	for _, want := range []string{"expected:", "observed:", "decoded frame", "test_expectation_mismatch"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("message %q missing structured failure detail %q", got.Message, want)
		}
	}
}

// TestTerminalObservationMalformedIsLoud proves the unresolvable-attribution
// path records class malformed_terminal with a LOUD, non-empty message naming
// what was missing — the deliberate signal a Slice 4 metric/alert fires on.
func TestTerminalObservationMalformedIsLoud(t *testing.T) {
	abort := string(decision.AbortMalformed)
	doc := runDoc{Attempts: []attemptDoc{{
		AttemptIndex: 0,
		Phase:        "llm-work",
		Conclusion:   stringPtrValue("failure"),
		Decision:     &abort,
	}}}
	wf := &server.Workflow{Phases: []server.PhaseSpec{{Name: "llm-work"}}}

	got := terminalObservationForRun(doc, wf, "aborted", nil, server.TerminalObservationSourceDecisionEngine)
	if got == nil || got.Class != server.TerminalObservationMalformed {
		t.Fatalf("want malformed_terminal, got %#v", got)
	}
	if strings.TrimSpace(got.Message) == "" {
		t.Fatal("malformed_terminal message must not be empty")
	}
	for _, want := range []string{"MALFORMED TERMINAL", "llm-work", "no job completions", "no verification verdict"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("message %q missing %q", got.Message, want)
		}
	}
}

func TestCanonicalExecutionFailureReasonRoundTripsDispatchReason(t *testing.T) {
	for _, reason := range []string{"dispatch_failed", "dispatch_timeout"} {
		if got := canonicalExecutionFailureReason(reason); got != reason {
			t.Fatalf("canonicalExecutionFailureReason(%q)=%q", reason, got)
		}
	}
}
