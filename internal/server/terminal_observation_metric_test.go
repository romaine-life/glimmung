package server

import "testing"

func TestTerminalObservationMetricClass(t *testing.T) {
	cases := []struct {
		name string
		obs  *RunTerminalObservation
		want string
	}{
		{name: "nil observation is the passed sentinel", obs: nil, want: TerminalObservationClassNone},
		{name: "verifier_failed passes through", obs: &RunTerminalObservation{Class: TerminalObservationVerifierFailed}, want: TerminalObservationVerifierFailed},
		{name: "malformed_terminal passes through", obs: &RunTerminalObservation{Class: TerminalObservationMalformed}, want: TerminalObservationMalformed},
		{name: "empty class collapses to unknown sentinel", obs: &RunTerminalObservation{Class: ""}, want: "unknown"},
		{name: "whitespace class collapses to unknown sentinel", obs: &RunTerminalObservation{Class: "   "}, want: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TerminalObservationMetricClass(tc.obs); got != tc.want {
				t.Fatalf("TerminalObservationMetricClass(%#v) = %q, want %q", tc.obs, got, tc.want)
			}
		})
	}
}

func TestTerminalObservationClassUnattributed(t *testing.T) {
	unattributed := []string{TerminalObservationMalformed, "unknown", " malformed_terminal "}
	for _, class := range unattributed {
		if !TerminalObservationClassUnattributed(class) {
			t.Errorf("class %q should be unattributed (alert must fire)", class)
		}
	}
	attributed := []string{
		TerminalObservationClassNone,
		TerminalObservationProducerPhaseFailed,
		TerminalObservationVerifierContractMissing,
		TerminalObservationVerifierFailed,
		TerminalObservationGateFailed,
		TerminalObservationDispatchFailed,
		TerminalObservationPhaseRequestedAbort,
		TerminalObservationManualAbort,
	}
	for _, class := range attributed {
		if TerminalObservationClassUnattributed(class) {
			t.Errorf("class %q is attributed and must NOT page", class)
		}
	}
}
