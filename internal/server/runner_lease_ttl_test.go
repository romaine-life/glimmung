package server

import "testing"

func ptrInt(value int) *int {
	return &value
}

func TestRunnerRunLeaseTTLSecondsUsesFourHourFloor(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{{
		Name: "quick",
		Jobs: []RunnerJobSpec{{ID: "quick", TimeoutSeconds: ptrInt(60)}},
	}}}

	if got := runnerLeaseTTLSeconds(wf); got != DefaultRunnerLeaseTTLSeconds {
		t.Fatalf("ttl=%d, want floor %d", got, DefaultRunnerLeaseTTLSeconds)
	}
}

func TestRunnerRunLeaseTTLSecondsSumsPhaseTimeoutsWithOverhead(t *testing.T) {
	wf := &Workflow{Phases: []PhaseSpec{
		{
			Name: "parallel",
			Jobs: []RunnerJobSpec{
				{ID: "short", TimeoutSeconds: ptrInt(600)},
				{ID: "long", TimeoutSeconds: ptrInt(7200)},
			},
		},
		{
			Name: "verify",
			Jobs: []RunnerJobSpec{{ID: "verify", TimeoutSeconds: ptrInt(5000)}},
		},
		{
			Name:    "merge",
			Kind:    "k8s_job",
			Purpose: PhasePurposeReviewGate,
			Jobs:    []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}},
		},
	}}

	want := runnerLeaseWorkflowOverheadSeconds +
		7200 + runnerLeasePhaseOverheadSeconds +
		5000 + runnerLeasePhaseOverheadSeconds +
		120 + runnerLeasePhaseOverheadSeconds
	if got := runnerLeaseTTLSeconds(wf); got != want {
		t.Fatalf("ttl=%d, want %d", got, want)
	}
}

func TestRunnerRunLeaseTTLSecondsCapsOpenEndedWorkflows(t *testing.T) {
	var phases []PhaseSpec
	for i := 0; i < 30; i++ {
		phases = append(phases, PhaseSpec{
			Name: "phase",
			Jobs: []RunnerJobSpec{{ID: "job"}},
		})
	}

	if got := runnerLeaseTTLSeconds(&Workflow{Phases: phases}); got != runnerLeaseMaxTTLSeconds {
		t.Fatalf("ttl=%d, want cap %d", got, runnerLeaseMaxTTLSeconds)
	}
}
