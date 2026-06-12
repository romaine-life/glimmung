package server

import (
	"context"
	"time"
)

const committedNativeLaunchTimeout = 2 * time.Minute

type NativeJobDispatchRecorder interface {
	RecordNativeJobsDispatched(ctx context.Context, project, runID, phase string, jobs map[string]string) error
}

func launchCommittedNativePhase(ctx context.Context, nativeLauncher NativeLauncher, req NativeLaunchRequest) ([]NativeLaunchedJob, error) {
	launchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), committedNativeLaunchTimeout)
	defer cancel()
	return nativeLauncher.LaunchNativePhase(launchCtx, req)
}

func recordLaunchedNativeJobs(ctx context.Context, store any, run RunReplayData, phase PhaseSpec, launched []NativeLaunchedJob) error {
	recorder, ok := store.(NativeJobDispatchRecorder)
	if !ok || recorder == nil {
		return nil
	}
	jobs := launchedNativeJobMap(launched)
	if len(jobs) == 0 {
		return nil
	}
	return recorder.RecordNativeJobsDispatched(ctx, run.Project, run.ID, phase.Name, jobs)
}

func launchedNativeJobMap(launched []NativeLaunchedJob) map[string]string {
	jobs := make(map[string]string, len(launched))
	for _, job := range launched {
		if job.JobID == "" || job.K8sJobName == "" {
			continue
		}
		jobs[job.JobID] = job.K8sJobName
	}
	return jobs
}
