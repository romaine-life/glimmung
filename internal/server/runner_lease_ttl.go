package server

import "fmt"

const (
	DefaultRunnerLeaseTTLSeconds = defaultIssueLockTTLSeconds

	runnerLeaseDefaultJobTimeoutSeconds = 60 * 60
	runnerLeaseWorkflowOverheadSeconds  = 30 * 60
	runnerLeasePhaseOverheadSeconds     = 5 * 60
	runnerLeaseMaxTTLSeconds            = 24 * 60 * 60
)

func runnerLeaseTTLSeconds(wf *Workflow) int {
	if wf == nil {
		return DefaultRunnerLeaseTTLSeconds
	}
	total := runnerLeaseWorkflowOverheadSeconds
	for _, phase := range wf.Phases {
		total += runnerPhaseLeaseSeconds(phase) + runnerLeasePhaseOverheadSeconds
	}
	if total < DefaultRunnerLeaseTTLSeconds {
		return DefaultRunnerLeaseTTLSeconds
	}
	if total > runnerLeaseMaxTTLSeconds {
		return runnerLeaseMaxTTLSeconds
	}
	return total
}

func runnerPhaseLeaseSeconds(phase PhaseSpec) int {
	jobs := CanonicalRunnerPhaseJobs(phase)
	if len(jobs) == 0 {
		return runnerLeaseDefaultJobTimeoutSeconds
	}
	phaseSeconds := 0
	for _, job := range jobs {
		jobSeconds := runnerLeaseDefaultJobTimeoutSeconds
		if job.TimeoutSeconds != nil && *job.TimeoutSeconds > 0 {
			jobSeconds = *job.TimeoutSeconds
		}
		if jobSeconds > phaseSeconds {
			phaseSeconds = jobSeconds
		}
	}
	return phaseSeconds
}

func runnerLeaseTTLP(value int) *int {
	return &value
}

func runnerLeaseNotClaimedError(lease Lease) error {
	ref := LeasePublicRefFromLease(lease)
	if ref == "" {
		ref = "<unknown>"
	}
	return fmt.Errorf("runner lease state is %q for %s, want claimed", lease.State, ref)
}
