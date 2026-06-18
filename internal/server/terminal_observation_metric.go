package server

import "strings"

// TerminalObservationClassNone is the sentinel class label for a run that
// reached a terminal state with no failure observation — i.e. a passed run.
// A clear sentinel keeps the metric's `class` label non-empty so operators
// never stare at a blank label row in Grafana, and so "passed" runs are
// distinguishable from an unattributed failure that collapsed to "unknown".
const TerminalObservationClassNone = "none"

// TerminalObservationMetricClass returns the bounded `class` label for the
// glimmung_run_terminal_total metric, derived from the guarded terminal
// observation. A nil observation (a passed run carries no failure cause) maps
// to the TerminalObservationClassNone sentinel rather than an empty label; an
// observation whose class somehow slipped through empty maps to "unknown" so it
// trips the unattributed-failure alert instead of vanishing into a blank label.
//
// The label space is bounded by construction: the closed RunTerminalObservation
// class enum, plus "none" (passed) and "unknown" (empty-class sentinel). Phase,
// job, and step identifiers are deliberately NOT folded into the label — they
// belong in the structured log, per the observability contract's no-unbounded-
// labels rule. Callers must pass the observation AFTER GuardTerminalFailureObservation
// so the label reflects the guarded, post-attribution class (a malformed
// terminal reads as "malformed_terminal", never the raw pre-guard class).
func TerminalObservationMetricClass(obs *RunTerminalObservation) string {
	if obs == nil {
		return TerminalObservationClassNone
	}
	if class := strings.TrimSpace(obs.Class); class != "" {
		return class
	}
	return "unknown"
}

// TerminalObservationClassUnattributed reports whether a terminal class is the
// unattributed-failure signal operators must be paged on: malformed_terminal
// (the guard's loud fallback when attribution could not be resolved) or
// "unknown" (an empty/unbounded class that slipped through). This is the exact
// predicate the GlimmungRunTerminalUnattributed alert mirrors at the metric
// layer, and the trigger for the loud structured drill-down log at the
// terminal-write choke point.
func TerminalObservationClassUnattributed(class string) bool {
	switch strings.TrimSpace(class) {
	case TerminalObservationMalformed, "unknown":
		return true
	default:
		return false
	}
}
