package server

import (
	"strings"
	"testing"
)

// TestGuardTerminalFailureObservationFailsClosed proves the fail-closed guard
// at the terminal-write choke point: a run may never settle into a terminal
// failure state with an unattributed cause. An absent observation, an
// empty/`unknown` class, or an empty message is rewritten to a
// malformed_terminal observation carrying a LOUD, non-empty message naming
// exactly what was missing — never a silent generic.
func TestGuardTerminalFailureObservationFailsClosed(t *testing.T) {
	reason := "operator-supplied abort reason"

	cases := []struct {
		name string
		obs  *RunTerminalObservation
	}{
		{name: "nil observation", obs: nil},
		{name: "empty class", obs: &RunTerminalObservation{Message: "something happened"}},
		{name: "unknown class", obs: &RunTerminalObservation{Class: "unknown", Message: "something happened"}},
		{name: "empty message", obs: &RunTerminalObservation{Class: TerminalObservationVerifierFailed, Phase: "llm-verify"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GuardTerminalFailureObservation(tc.obs, "aborted", TerminalObservationSourceDecisionEngine, &reason)
			if got == nil {
				t.Fatal("guard returned nil for a terminal failure write")
			}
			if got.Class != TerminalObservationMalformed {
				t.Fatalf("class=%q want malformed_terminal", got.Class)
			}
			if strings.TrimSpace(got.Message) == "" {
				t.Fatal("malformed_terminal message must be loud, not empty")
			}
			if !strings.Contains(got.Message, "MALFORMED TERMINAL") {
				t.Fatalf("message %q must be loud", got.Message)
			}
			if !strings.Contains(got.Message, reason) {
				t.Fatalf("message %q must name the abort_reason that was present", got.Message)
			}
			// Partial owner identity is preserved when present.
			if tc.obs != nil && tc.obs.Phase != "" && got.Phase != tc.obs.Phase {
				t.Fatalf("phase=%q want preserved %q", got.Phase, tc.obs.Phase)
			}
		})
	}
}

// TestGuardTerminalFailureObservationPassesWellFormed proves the guard leaves a
// well-formed observation untouched — including a malformed_terminal that
// already carries a loud message (the deliberate "attribution unresolved"
// signal must survive so the metric can fire on it).
func TestGuardTerminalFailureObservationPassesWellFormed(t *testing.T) {
	wellFormed := &RunTerminalObservation{
		Class:   TerminalObservationVerifierFailed,
		Phase:   "llm-verify",
		JobID:   "llm-verify",
		Reason:  "claimed_result_not_observed",
		Message: "verification phase llm-verify failed at job llm-verify: claimed_result_not_observed",
	}
	if got := GuardTerminalFailureObservation(wellFormed, "aborted", TerminalObservationSourceCompletionCallback, nil); got != wellFormed {
		t.Fatalf("well-formed observation must pass through unchanged, got %#v", got)
	}

	loudMalformed := &RunTerminalObservation{
		Class:   TerminalObservationMalformed,
		Message: "MALFORMED TERMINAL: run aborted on phase \"x\" without a resolvable typed cause — no verification verdict",
	}
	if got := GuardTerminalFailureObservation(loudMalformed, "aborted", TerminalObservationSourceDecisionEngine, nil); got != loudMalformed {
		t.Fatalf("loud malformed_terminal must pass through unchanged, got %#v", got)
	}
}

// TestGuardTerminalFailureObservationIgnoresNonFailureStates proves a
// successful terminal state (e.g. "passed") is never coerced into a malformed
// observation, and a nil observation stays nil.
func TestGuardTerminalFailureObservationIgnoresNonFailureStates(t *testing.T) {
	if got := GuardTerminalFailureObservation(nil, "passed", TerminalObservationSourceCompletionCallback, nil); got != nil {
		t.Fatalf("passed state must not synthesize an observation, got %#v", got)
	}
	if !TerminalFailureState("aborted") || !TerminalFailureState("failed") {
		t.Fatal("aborted and failed must be terminal failure states")
	}
	if TerminalFailureState("passed") || TerminalFailureState("in_progress") {
		t.Fatal("non-failure states must not be classified as terminal failures")
	}
}
