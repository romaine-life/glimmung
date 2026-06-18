package server

import (
	"fmt"
	"strings"
)

// RunTerminalObservation is the typed, durable explanation for why a run
// reached a terminal state. AbortReason remains the human-readable summary;
// this record is the machine-readable source of truth for operators and
// follow-up agents.
type RunTerminalObservation struct {
	Class      string `json:"class"`
	Phase      string `json:"phase,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	StepSlug   string `json:"step_slug,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Reason     string `json:"reason,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Source     string `json:"source"`
	Message    string `json:"message"`
}

const (
	TerminalObservationProducerPhaseFailed     = "producer_phase_failed"
	TerminalObservationVerifierContractMissing = "verifier_contract_missing"
	TerminalObservationVerifierFailed          = "verifier_failed"
	TerminalObservationGateFailed              = "gate_failed"
	TerminalObservationDispatchFailed          = "dispatch_failed"
	TerminalObservationPhaseRequestedAbort     = "phase_requested_abort"
	TerminalObservationManualAbort             = "manual_abort"
	TerminalObservationMalformed               = "malformed_terminal"
)

// AllTerminalObservationClasses is the single canonical source of truth for
// every terminal-failure observation class the platform recognizes. It is
// co-located with the enum above on purpose: the two must never drift.
//
// The platform invariant is enforced across three layers per terminal failure
// class — attribution (terminalObservationForRun / GuardTerminalFailureObservation),
// run-graph owner-step projection (ensureFailedJobOwnerStep), and inspector
// render (the .run-failure-cause block). This list is the regression backstop
// that keeps those three layers honest as the system grows: EVERY
// TerminalObservation* class const MUST appear here exactly once, and every
// entry here MUST be covered by the inventory tests
// (TestAllTerminalObservationClassesInventoryIsExact connects the list to the
// named consts; TestTerminalObservationFixtureCoversEveryCanonicalClass and
// TestEveryTerminalClassProjectsAFailedOwnerStep connect it to attribution and
// projection coverage; the frontend mirror proves each class renders a cause).
//
// Adding a ninth terminal class is therefore a conscious, tested act: a new
// const that is not added here (or an entry here with no const) fails the
// tripwire, and a class with no attribution/projection/render fixture fails the
// inventory tests. That is the structural reason a future failure class cannot
// slip through unattributed or invisible.
var AllTerminalObservationClasses = []string{
	TerminalObservationProducerPhaseFailed,
	TerminalObservationVerifierContractMissing,
	TerminalObservationVerifierFailed,
	TerminalObservationGateFailed,
	TerminalObservationDispatchFailed,
	TerminalObservationPhaseRequestedAbort,
	TerminalObservationManualAbort,
	TerminalObservationMalformed,
}

const (
	TerminalObservationSourceCompletionCallback = "completion_callback"
	TerminalObservationSourceAdminAbort         = "admin_abort"
	TerminalObservationSourceDecisionEngine     = "decision_engine"
)

// TerminalFailureStates are the durable run states that represent a terminal
// non-success outcome. The platform invariant is that no run may settle into
// one of these states without a typed terminal observation that names a known
// non-success cause with a specific reason and a non-empty message. "aborted"
// is the run-level terminal failure state today; "failed" is listed so the
// guard stays correct if a future state is added.
func TerminalFailureState(state string) bool {
	switch strings.TrimSpace(state) {
	case "aborted", "failed":
		return true
	default:
		return false
	}
}

// terminalObservationAttributionGaps reports the ways a terminal observation
// fails the attribution invariant for a terminal-failure write. An empty slice
// means the observation is well-formed: a known, non-success class plus a
// non-empty message. A malformed_terminal observation that already carries a
// loud (non-empty) message is the deliberate "attribution unresolved" signal
// and is NOT a gap — it is allowed through so the Slice 4 metric/alert can fire
// on it. The forbidden states are: no observation at all, an empty/`unknown`
// class, a malformed class with no message, or any observation with an empty
// message.
func terminalObservationAttributionGaps(obs *RunTerminalObservation) []string {
	if obs == nil {
		return []string{"no terminal observation was produced"}
	}
	gaps := []string{}
	switch strings.TrimSpace(obs.Class) {
	case "":
		gaps = append(gaps, "terminal class is empty")
	case "unknown":
		gaps = append(gaps, `terminal class is "unknown"`)
	}
	if strings.TrimSpace(obs.Message) == "" {
		gaps = append(gaps, "terminal message is empty")
	}
	return gaps
}

// GuardTerminalFailureObservation is the fail-closed guard at the terminal-write
// choke point. A run may never be persisted into a terminal failure state with
// an unattributed cause: an absent observation, an empty/`unknown` class, or an
// empty message. When the supplied observation is well-formed it is returned
// unchanged. When attribution cannot be resolved, the guard returns a
// malformed_terminal observation with a LOUD message naming exactly what was
// missing — never a silent generic — preserving whatever partial owner identity
// was resolved. Non-failure states (e.g. "passed") pass through untouched.
func GuardTerminalFailureObservation(obs *RunTerminalObservation, state, source string, abortReason *string) *RunTerminalObservation {
	if !TerminalFailureState(state) {
		return obs
	}
	gaps := terminalObservationAttributionGaps(obs)
	if len(gaps) == 0 {
		return obs
	}
	loud := fmt.Sprintf(
		"MALFORMED TERMINAL: run reached terminal %q without a well-formed terminal observation (%s); attribution could not be resolved",
		strings.TrimSpace(state),
		strings.Join(gaps, ", "),
	)
	if abortReason != nil {
		if reason := strings.TrimSpace(*abortReason); reason != "" {
			loud += "; abort_reason was: " + reason
		}
	}
	guarded := &RunTerminalObservation{
		Class:   TerminalObservationMalformed,
		Reason:  "malformed_terminal",
		Source:  firstNonEmpty(source, TerminalObservationSourceDecisionEngine),
		Message: loud,
	}
	if obs != nil {
		guarded.Phase = obs.Phase
		guarded.JobID = obs.JobID
		guarded.StepSlug = obs.StepSlug
		guarded.Conclusion = obs.Conclusion
	}
	return guarded
}
