package server

import "testing"

// TestAllTerminalObservationClassesInventoryIsExact is the TRIPWIRE that keeps
// the canonical AllTerminalObservationClasses list in lockstep with the
// TerminalObservation* class consts. It names every known class const
// explicitly here, then asserts the canonical list equals that known set with
// no duplicates and no omissions.
//
// Why this is load-bearing: AllTerminalObservationClasses is the single source
// of truth the attribution, projection, and render inventory tests iterate. If
// a developer adds a ninth class const and wires it into the canonical list but
// forgets to teach this test about it (or vice-versa — adds it here but not to
// the list), the two sets diverge and CI fails with a precise message. Adding a
// terminal failure class is, by construction, a conscious and tested act.
func TestAllTerminalObservationClassesInventoryIsExact(t *testing.T) {
	// The known set of class consts, named explicitly. This is the second,
	// independent enumeration the canonical list is checked against — editing
	// one without the other trips the guard.
	known := []string{
		TerminalObservationProducerPhaseFailed,
		TerminalObservationVerifierContractMissing,
		TerminalObservationVerifierFailed,
		TerminalObservationGateFailed,
		TerminalObservationDispatchFailed,
		TerminalObservationPhaseRequestedAbort,
		TerminalObservationManualAbort,
		TerminalObservationMalformed,
	}

	// No duplicates and no empty entries in the canonical list itself.
	seen := map[string]bool{}
	for _, class := range AllTerminalObservationClasses {
		if class == "" {
			t.Fatalf("AllTerminalObservationClasses contains an empty class string")
		}
		if seen[class] {
			t.Fatalf("AllTerminalObservationClasses contains duplicate class %q", class)
		}
		seen[class] = true
	}

	knownSet := map[string]bool{}
	for _, class := range known {
		if class == "" {
			t.Fatalf("known class set contains an empty string")
		}
		if knownSet[class] {
			t.Fatalf("known class set contains duplicate class %q", class)
		}
		knownSet[class] = true
	}

	if len(AllTerminalObservationClasses) != len(known) {
		t.Fatalf(
			"AllTerminalObservationClasses has %d classes, the known const set has %d — add the new class to BOTH the canonical list and this tripwire",
			len(AllTerminalObservationClasses), len(known),
		)
	}

	// Every canonical class is a known const.
	for _, class := range AllTerminalObservationClasses {
		if !knownSet[class] {
			t.Fatalf("class %q is in AllTerminalObservationClasses but not in this tripwire's known const set — name it explicitly here", class)
		}
	}
	// Every known const is in the canonical list.
	for _, class := range known {
		if !seen[class] {
			t.Fatalf("class const %q is not in AllTerminalObservationClasses — add it to the canonical source-of-truth list", class)
		}
	}
}

// terminalClassProjectionFixture is the per-class projection shape used by the
// enum-driven inventory test: a job projected in a terminal-failure state that
// carries the class's representative reason. The invariant being guarded is that
// such a job can never render with every step succeeded/skipped/not_started —
// ensureFailedJobOwnerStep (slice 2) must synthesize or surface a `failed` owner
// step. neverRan models the dispatch-shaped case where the job never executed.
type terminalClassProjectionFixture struct {
	jobState string // "failed" or "aborted"
	reason   string
	neverRan bool
}

// terminalClassProjectionFixtures maps EVERY canonical terminal class to a
// projection fixture. It is intentionally keyed by class so that a class added
// to AllTerminalObservationClasses without a fixture here fails the inventory
// test below rather than being silently skipped.
func terminalClassProjectionFixtures() map[string]terminalClassProjectionFixture {
	return map[string]terminalClassProjectionFixture{
		// A producer phase whose job failed: steps ran, verdict owner synthesized.
		TerminalObservationProducerPhaseFailed: {jobState: "failed", reason: "job_failed"},
		// Verifier contract missing: the verify job failed with no usable verdict.
		TerminalObservationVerifierContractMissing: {jobState: "failed", reason: "verification_contract_missing"},
		// Verifier returned a failing verdict while every step exited 0.
		TerminalObservationVerifierFailed: {jobState: "failed", reason: "verification_failed"},
		// Evidence gate failed its required-evidence check.
		TerminalObservationGateFailed: {jobState: "failed", reason: "evidence_gate_failed"},
		// Forward dispatch failed: the job never ran, dispatch owner synthesized.
		TerminalObservationDispatchFailed: {jobState: "failed", reason: "dispatch_failed", neverRan: true},
		// A phase requested a fail-closed abort.
		TerminalObservationPhaseRequestedAbort: {jobState: "aborted", reason: "phase_requested_abort"},
		// Operator manually aborted the run.
		TerminalObservationManualAbort: {jobState: "aborted", reason: "manual_abort"},
		// Unresolvable attribution — the loud malformed-terminal signal.
		TerminalObservationMalformed: {jobState: "failed", reason: "malformed_terminal"},
	}
}

// TestEveryTerminalClassProjectsAFailedOwnerStep is the enum-driven PROJECTION
// inventory: for EVERY class in the canonical list, a job projected in that
// class's terminal-failure shape must own a `failed` step. It reuses slice 2's
// ensureFailedJobOwnerStep (the projection logic) and assertFailedJobsOwnAFailedStep
// (the invariant assertion), and connects them to AllTerminalObservationClasses
// so a future class with no projection fixture fails CI instead of slipping
// through invisible.
func TestEveryTerminalClassProjectsAFailedOwnerStep(t *testing.T) {
	fixtures := terminalClassProjectionFixtures()

	// Reject drift in both directions: a fixture for a non-canonical class is a
	// stale entry that must be removed.
	for class := range fixtures {
		found := false
		for _, canonical := range AllTerminalObservationClasses {
			if canonical == class {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("projection fixture exists for %q which is not in AllTerminalObservationClasses", class)
		}
	}

	greenSteps := func() []RunProjectionStep {
		return []RunProjectionStep{
			{Slug: "build-and-deploy", State: "succeeded"},
			{Slug: "run-verification", State: "succeeded"},
			{Slug: "finalize-verification", State: "succeeded"},
		}
	}

	for _, class := range AllTerminalObservationClasses {
		class := class
		t.Run(class, func(t *testing.T) {
			fixture, ok := fixtures[class]
			if !ok {
				t.Fatalf("terminal class %q has no projection fixture — every canonical class MUST be covered so it can never render as a failed job with no failed owner step", class)
			}
			reason := fixture.reason
			steps := ensureFailedJobOwnerStep(fixture.jobState, &reason, greenSteps(), fixture.neverRan)

			run := RunProjectionRun{
				Phases: []RunProjectionPhase{{
					Name:   "terminal-phase",
					Kind:   "k8s_job",
					State:  fixture.jobState,
					Reason: &reason,
					Jobs: []RunProjectionJob{{
						ID:     "terminal-job",
						State:  fixture.jobState,
						Reason: &reason,
						Steps:  steps,
					}},
				}},
			}

			// Slice 2's invariant assertion: no failed/aborted job may render
			// with every step succeeded/skipped/not_started.
			assertFailedJobsOwnAFailedStep(t, run)
		})
	}
}
