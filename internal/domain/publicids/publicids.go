package publicids

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidRunCycleAddress is returned when a string is not a canonical
// run-cycle address.
var ErrInvalidRunCycleAddress = errors.New("invalid run cycle address")

// RunCycleAddress is the canonical, unambiguous address of a single run cycle:
// the logical run number and the run-local cycle ordinal, rendered "run.cycle"
// (e.g. "6.1").
//
// It is deliberately NOT the flat issue-scoped cycle ledger number
// (runDoc.CycleNumber) and NOT a read-time positional ordinal. Addressing a run
// by either of those was the defect behind get_run_report(run_number=9)
// returning the run that displays as "6.1": "9" was the cycle ledger of the
// run whose canonical address is "6.1". A run cycle has exactly one canonical
// address — its run_display_number — and that is the only thing the resolvers
// accept.
type RunCycleAddress struct {
	Run   int
	Cycle int
}

// String renders the canonical "run.cycle" form.
func (a RunCycleAddress) String() string {
	return strconv.Itoa(a.Run) + "." + strconv.Itoa(a.Cycle)
}

// ParseRunCycleAddress parses the canonical "run.cycle" address from a single
// token. It requires exactly two positive integers separated by a single dot.
// A bare run number, a cycle-ledger integer, an empty string, a zero/negative
// component, or any non-numeric input is rejected with ErrInvalidRunCycleAddress
// so an ambiguous token can never silently resolve to the wrong run cycle.
func ParseRunCycleAddress(s string) (RunCycleAddress, error) {
	trimmed := strings.TrimSpace(s)
	parts := strings.Split(trimmed, ".")
	if len(parts) != 2 {
		return RunCycleAddress{}, fmt.Errorf("%w: %q (expected \"run.cycle\", e.g. \"6.1\")", ErrInvalidRunCycleAddress, s)
	}
	run, okRun := positiveInt(parts[0])
	cycle, okCycle := positiveInt(parts[1])
	if !okRun || !okCycle {
		return RunCycleAddress{}, fmt.Errorf("%w: %q (expected \"run.cycle\", e.g. \"6.1\")", ErrInvalidRunCycleAddress, s)
	}
	return RunCycleAddress{Run: run, Cycle: cycle}, nil
}

// ParseRunCycleSegments builds the canonical address from already-split route
// segments — the /runs/{run_number}/cycles/{cycle_number} path. Both segments
// must be positive integers and the run segment must be the bare run number
// (the dotted form belongs to the single-segment routes).
func ParseRunCycleSegments(runSegment, cycleSegment string) (RunCycleAddress, error) {
	runSegment = strings.TrimSpace(runSegment)
	cycleSegment = strings.TrimSpace(cycleSegment)
	if strings.Contains(runSegment, ".") {
		return RunCycleAddress{}, fmt.Errorf("%w: run segment %q must be the bare run number when a cycle segment is present", ErrInvalidRunCycleAddress, runSegment)
	}
	run, okRun := positiveInt(runSegment)
	cycle, okCycle := positiveInt(cycleSegment)
	if !okRun || !okCycle {
		return RunCycleAddress{}, fmt.Errorf("%w: runs/%q/cycles/%q (both must be positive integers)", ErrInvalidRunCycleAddress, runSegment, cycleSegment)
	}
	return RunCycleAddress{Run: run, Cycle: cycle}, nil
}

func positiveInt(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// IssueRef returns the operator-facing issue reference for a project issue.
func IssueRef(project string, number *int) string {
	if number == nil {
		return project
	}
	return project + "#" + strconv.Itoa(*number)
}

// RunRef returns the operator-facing issue-scoped run reference.
func RunRef(project string, issueNumber int, runDisplay string) string {
	if issueNumber <= 0 {
		panic("run issue number must be positive")
	}
	runPart := strings.TrimSpace(runDisplay)
	if runPart == "" {
		runPart = "unknown"
	}
	return project + "#" + strconv.Itoa(issueNumber) + "/runs/" + runPart
}

// ReviewRef returns the operator-facing review or pull request reference.
func ReviewRef(repo string, number *int) string {
	if number == nil {
		return repo
	}
	return repo + "#" + strconv.Itoa(*number)
}

// LeaseRef returns the operator-facing lease reference.
func LeaseRef(project, slotName string, leaseNumber *int) string {
	if slotName != "" {
		return slotName
	}
	if leaseNumber != nil {
		return project + "/leases/" + strconv.Itoa(*leaseNumber)
	}
	return project + "/lease"
}
