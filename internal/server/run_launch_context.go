package server

import (
	"context"
	"time"
)

const committedRunLaunchTimeout = 2 * time.Minute

type RunnerJobDispatchRecorder interface {
	RecordRunnerJobsDispatched(ctx context.Context, project, runID, phase string, jobs map[string]string) error
}

func launchCommittedPhase(ctx context.Context, runLauncher RunLauncher, req RunLaunchRequest) ([]RunLaunchedJob, error) {
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedRunLaunchTimeout)
	defer cancel()
	return runLauncher.LaunchPhase(launchCtx, req)
}

func recordLaunchedRunnerJobs(ctx context.Context, store any, run RunReplayData, phase PhaseSpec, launched []RunLaunchedJob) error {
	recorder, ok := store.(RunnerJobDispatchRecorder)
	if !ok || recorder == nil {
		return nil
	}
	jobs := launchedRunnerJobMap(launched)
	if len(jobs) == 0 {
		return nil
	}
	return recorder.RecordRunnerJobsDispatched(ctx, run.Project, run.ID, phase.Name, jobs)
}

func launchedRunnerJobMap(launched []RunLaunchedJob) map[string]string {
	jobs := make(map[string]string, len(launched))
	for _, job := range launched {
		if job.JobID == "" || job.K8sJobName == "" {
			continue
		}
		jobs[job.JobID] = job.K8sJobName
	}
	return jobs
}
