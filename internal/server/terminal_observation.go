package server

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

const (
	TerminalObservationSourceCompletionCallback = "completion_callback"
	TerminalObservationSourceAdminAbort         = "admin_abort"
	TerminalObservationSourceDecisionEngine     = "decision_engine"
)
