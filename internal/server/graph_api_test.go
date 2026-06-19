package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fakeGraphStore struct {
	fakeReadStore
	issue       IssueDetail
	issues      []IssueRow
	runs        []RunReport
	reviews []ReviewRow
	signals     []GraphSignal
	runnerLogs  RunnerLogsResponse
}

func (s fakeGraphStore) ListIssues(context.Context, IssueListFilter) ([]IssueRow, error) {
	return s.issues, nil
}

func (s fakeGraphStore) GetIssueDetailByNumber(_ context.Context, project string, number int) (IssueDetail, error) {
	if s.issue.Project == project && s.issue.Number != nil && *s.issue.Number == number {
		return s.issue, nil
	}
	return IssueDetail{}, ErrNotFound
}

func (s fakeGraphStore) ArchiveIssueByNumber(context.Context, IssueArchive) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) CreateIssue(context.Context, IssueCreate) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) PatchIssueByNumber(context.Context, IssuePatch) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) AddIssueComment(context.Context, IssueCommentAdd) (IssueComment, error) {
	return IssueComment{}, ErrUnsupported
}

func (s fakeGraphStore) UpdateIssueComment(context.Context, IssueCommentUpdate) (IssueComment, error) {
	return IssueComment{}, ErrUnsupported
}

func (s fakeGraphStore) DeleteIssueComment(context.Context, IssueCommentDelete) (IssueDetail, error) {
	return IssueDetail{}, ErrUnsupported
}

func (s fakeGraphStore) ListProjectRuns(_ context.Context, project string, _ int) ([]RunReport, error) {
	out := make([]RunReport, 0, len(s.runs))
	for _, run := range s.runs {
		if run.Project == project {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s fakeGraphStore) GetRunReportByNumber(context.Context, string, int, string) (RunReport, error) {
	return RunReport{}, ErrUnsupported
}

func (s fakeGraphStore) ListReviews(_ context.Context, filter ReviewListFilter) ([]ReviewRow, error) {
	out := make([]ReviewRow, 0, len(s.reviews))
	for _, row := range s.reviews {
		if filter.Project == "" || row.Project == filter.Project {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s fakeGraphStore) GetReviewForIssue(context.Context, string, int) (ReviewDetail, error) {
	return ReviewDetail{}, ErrUnsupported
}

func (s fakeGraphStore) EnsureReview(context.Context, ReviewCreate) (ReviewDetail, error) {
	return ReviewDetail{}, ErrUnsupported
}

func (s fakeGraphStore) ListGraphSignals(context.Context, GraphSignalFilter) ([]GraphSignal, error) {
	return s.signals, nil
}

func (s fakeGraphStore) GetRunnerStatusByID(context.Context, string, string) (RunnerStatusResponse, error) {
	return RunnerStatusResponse{}, ErrUnsupported
}

func (s fakeGraphStore) RecordRunnerEventByID(context.Context, string, string, RunnerEventRequest) (RunnerEventResult, error) {
	return RunnerEventResult{}, ErrUnsupported
}

func (s fakeGraphStore) ListRunnerEventsByID(context.Context, string, string, *int, *string, *string, *int, *int) (RunnerLogsResponse, error) {
	return s.runnerLogs, nil
}

func TestIssueGraphByNumberBuildsRunAttemptAndReviewNodes(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	runDisplay := "1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1"
	reviewRef := "romaine-life/glimmung#452"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{
					Name:    "env-prep",
					Kind:    "k8s_job",
					Outputs: []string{"validation_url"},
					Jobs: []RunnerJobSpec{{
						ID:   "prepare",
						Name: stringPtr("prepare env"),
						Steps: []RunnerStepSpec{{
							Slug:  "checkout",
							Title: stringPtr("checkout"),
						}},
					}},
				},
				{Name: "agent-execute", DependsOn: []string{"env-prep"}},
			},
			PR: PrPrimitive{},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
			Labels:  []string{"backend"},
		},
		runs: []RunReport{{
			ID:                "run-1",
			Project:           "glimmung",
			RunRef:            runRef,
			RunNumber:         &runNumber,
			RunDisplayNumber:  &runDisplay,
			Workflow:          "agent-run",
			IssueRef:          "glimmung#17",
			IssueNumber:       issueNumber,
			State:             "in_progress",
			CurrentPhase:      stringPtr("agent-execute"),
			ValidationURL:     stringPtr("https://preview.example"),
			CumulativeCostUSD: 1.25,
			StartedAt:         now,
			UpdatedAt:         now,
			Attempts: []RunReportAttempt{{
				AttemptIndex:       0,
				Phase:              "env-prep",
				PhaseKind:          "k8s_job",
				WorkflowFilename:   "k8s_job:env-prep",
				DispatchedAt:       now,
				CompletedAt:        &now,
				Conclusion:         stringPtr("success"),
				VerificationStatus: stringPtr("pass"),
				EvidenceRefs:       []string{"blob://artifacts/glimmung/17/verification.json"},
				Evidence: []EvidenceArtifact{{
					Kind:               "video",
					Ref:                "videos/release-pulse.webm",
					Label:              "release pulse",
					SourcePhase:        "env-prep",
					SourceAttemptIndex: intPtr(0),
				}},
				LogArchiveURL: stringPtr("blob://artifacts/glimmung/17/run.log"),
				PhaseOutputs:  map[string]string{"validation_url": "https://preview.example"},
				JobCompletions: []RunAttemptJobCompletion{{
					JobID:              "prepare",
					CompletedAt:        &now,
					Conclusion:         "success",
					VerificationStatus: stringPtr("pass"),
					CostUSD:            0.8125,
				}},
			}},
		}},
		reviews: []ReviewRow{{
			Ref:          reviewRef,
			Project:      "glimmung",
			Repo:         "romaine-life/glimmung",
			PRNumber:     452,
			Title:        "graph port",
			State:        "ready",
			HTMLURL:      stringPtr("https://github.com/romaine-life/glimmung/pull/452"),
			LinkedRunRef: stringPtr(runRef),
			Evidence: []ReviewEvidence{{
				Kind:         "screenshot",
				Ref:          "blob://artifacts/runs/glimmung/run-1/screenshots/default.png",
				Label:        "default",
				URL:          "/v1/artifacts/runs/glimmung/run-1/screenshots/default.png",
				ArtifactPath: "runs/glimmung/run-1/screenshots/default.png",
			}},
		}},
		signals: []GraphSignal{{
			ID:         "sig-1",
			TargetType: "run",
			TargetRepo: "glimmung",
			TargetID:   runRef,
			Source:     "glimmung_ui",
			State:      "pending",
			EnqueuedAt: now.Add(time.Minute),
			Payload:    map[string]any{"kind": "reject"},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var graph IssueGraph
	getJSON(t, handler, "/v1/issues/by-number/glimmung/17/graph", &graph)

	if graph.IssueRef != "glimmung#17" {
		t.Fatalf("issue_ref=%q", graph.IssueRef)
	}
	assertGraphNode(t, graph, "issue:glimmung#17", "issue")
	runNode := assertGraphNode(t, graph, "run:"+runRef, "run")
	if runNode.Metadata["workflow"].(string) != "agent-run" {
		t.Fatalf("run metadata=%#v", runNode.Metadata)
	}
	if _, ok := runNode.Metadata["workflow_graph"]; ok {
		t.Fatalf("run metadata should not carry retired workflow_graph topology fallback: %#v", runNode.Metadata)
	}
	assertGraphNode(t, graph, "attempt:"+runRef+":0", "attempt")
	attemptNode := assertGraphNode(t, graph, "attempt:"+runRef+":0", "attempt")
	if got, ok := attemptNode.Metadata["jobs_count"].(float64); !ok || got != 1 {
		t.Fatalf("attempt jobs_count=%#v", got)
	}
	assertGraphNode(t, graph, "pr:"+reviewRef, "pr")
	assertGraphEdge(t, graph, "run:"+runRef, "pr:"+reviewRef, "opened")
	assertGraphEdge(t, graph, "run:"+runRef, "signal:glimmung_ui:"+runRef+":"+now.Add(time.Minute).Format(time.RFC3339Nano), "feedback")
	if graph.Projection.IssueRef != "glimmung#17" {
		t.Fatalf("projection issue_ref=%q", graph.Projection.IssueRef)
	}
	if graph.Projection.CurrentRunRef == nil || *graph.Projection.CurrentRunRef != runRef {
		t.Fatalf("current_run_ref=%#v", graph.Projection.CurrentRunRef)
	}
	if graph.Projection.NextAction.Kind != "feedback_pending" {
		t.Fatalf("next action=%#v", graph.Projection.NextAction)
	}
	assertProjectionEdge(t, graph.Projection, "run:"+runRef, "phase:"+runRef+":env-prep", "contains")
	assertProjectionEdge(t, graph.Projection, "phase:"+runRef+":env-prep", "phase:"+runRef+":agent-execute", "depends_on")
	if len(graph.Projection.Runs) != 1 {
		t.Fatalf("projection runs=%#v", graph.Projection.Runs)
	}
	envPhase := assertProjectionPhase(t, graph.Projection.Runs[0], "env-prep")
	if envPhase.State != "succeeded" || len(envPhase.Jobs) != 1 || envPhase.Jobs[0].State != "succeeded" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].Conclusion == nil || *envPhase.Jobs[0].Conclusion != "success" || envPhase.Jobs[0].CompletedAt == nil {
		t.Fatalf("env-prep job completion=%#v", envPhase.Jobs[0])
	}
	if envPhase.Jobs[0].CostUSD == nil || *envPhase.Jobs[0].CostUSD != 0.8125 {
		t.Fatalf("env-prep job cost=%#v", envPhase.Jobs[0].CostUSD)
	}
	if len(envPhase.Jobs[0].Steps) != 1 || envPhase.Jobs[0].Steps[0].Slug != "checkout" {
		t.Fatalf("env-prep job steps=%#v", envPhase.Jobs[0].Steps)
	}
	executePhase := assertProjectionPhase(t, graph.Projection.Runs[0], "agent-execute")
	if executePhase.State != "dispatching" {
		t.Fatalf("agent-execute state=%q", executePhase.State)
	}
	assertProjectionEvidence(t, graph.Projection.Runs[0], "validation", "https://preview.example")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "artifact", "blob://artifacts/glimmung/17/verification.json")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "log", "blob://artifacts/glimmung/17/run.log")
	assertProjectionEvidence(t, graph.Projection.Runs[0], "screenshot", "blob://artifacts/runs/glimmung/run-1/screenshots/default.png")
	videoEvidence := findProjectionEvidence(t, graph.Projection.Runs[0], "video", "videos/release-pulse.webm")
	if videoEvidence.URL == nil || *videoEvidence.URL != "/v1/artifacts/runs/glimmung/run-1/videos/release-pulse.webm" {
		t.Fatalf("video evidence URL=%#v", videoEvidence.URL)
	}
	if videoEvidence.ArtifactPath != "runs/glimmung/run-1/videos/release-pulse.webm" {
		t.Fatalf("video evidence artifact_path=%q", videoEvidence.ArtifactPath)
	}
	if videoEvidence.SourcePhase != "env-prep" || videoEvidence.SourceAttemptIndex == nil || *videoEvidence.SourceAttemptIndex != 0 {
		t.Fatalf("video evidence source phase/index=%q/%#v", videoEvidence.SourcePhase, videoEvidence.SourceAttemptIndex)
	}
	if videoEvidence.VerificationStatus != "pass" {
		t.Fatalf("video evidence verification_status=%q", videoEvidence.VerificationStatus)
	}
	refEvidence := findProjectionEvidence(t, graph.Projection.Runs[0], "artifact", "blob://artifacts/glimmung/17/verification.json")
	if refEvidence.VerificationStatus != "pass" || refEvidence.SourcePhase != "env-prep" || refEvidence.SourceAttemptIndex == nil || *refEvidence.SourceAttemptIndex != 0 {
		t.Fatalf("ref evidence verification/source=%q/%q/%#v", refEvidence.VerificationStatus, refEvidence.SourcePhase, refEvidence.SourceAttemptIndex)
	}
	assertProjectionEvidence(t, graph.Projection.Runs[0], "pull_request", "https://github.com/romaine-life/glimmung/pull/452")
	if len(graph.Projection.Signals) != 1 || graph.Projection.Signals[0].Kind != "reject" {
		t.Fatalf("projection signals=%#v", graph.Projection.Signals)
	}
}

func TestRunCycleGraphProjectionUsesCanonicalStateAndNativeActivity(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{
					Name: "env-prep",
					Kind: "k8s_job",
					Jobs: []RunnerJobSpec{{
						ID:   "prepare",
						Name: stringPtr("prepare env"),
						Steps: []RunnerStepSpec{{
							Slug:  "checkout",
							Title: stringPtr("checkout"),
						}},
					}},
				},
				{
					Name:      "agent-execute",
					Kind:      "k8s_job",
					DependsOn: []string{"env-prep"},
					RecyclePolicy: &RecyclePolicy{
						MaxAttempts: 3,
						On:          []string{"verify_fail", "verify_malformed"},
						LandsAt:     "self",
					},
					Jobs: []RunnerJobSpec{{ID: "agent"}},
				},
			},
			PR: PrPrimitive{},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:                  "run-1",
			Project:             "glimmung",
			RunRef:              runRef,
			RunNumber:           &runNumber,
			CycleNumber:         &cycleNumber,
			RunCycleNumber:      &runCycle,
			RunDisplayNumber:    &runDisplay,
			Workflow:            "agent-run",
			IssueRef:            "glimmung#17",
			IssueNumber:         issueNumber,
			State:               "in_progress",
			CurrentPhase:        stringPtr("env-prep"),
			CumulativeCostUSD:   0,
			StartedAt:           now,
			UpdatedAt:           now,
			AttemptsCount:       1,
			ValidationURL:       nil,
			ScreenshotsMarkdown: nil,
			Attempts: []RunReportAttempt{{
				AttemptIndex:     0,
				Phase:            "env-prep",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:env-prep",
				DispatchedAt:     now,
			}},
		}},
		runnerLogs: RunnerLogsResponse{Events: []RunnerLogEvent{{
			Project:      "glimmung",
			RunRef:       runRef,
			AttemptIndex: 0,
			Phase:        "env-prep",
			JobID:        "prepare",
			Seq:          1,
			Event:        "step_started",
			StepSlug:     "checkout",
			CreatedAt:    now.Format(time.RFC3339Nano),
		}}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	if len(projection.Runs) != 1 {
		t.Fatalf("projection runs=%#v", projection.Runs)
	}
	if got := projection.Runs[0].Topology.RecycleArrows; len(got) != 1 ||
		got[0].Source != "agent-execute" ||
		got[0].Target != "agent-execute" ||
		got[0].Kind != "phase_recycle" ||
		got[0].Trigger != "verify_fail / verify_malformed" ||
		got[0].MaxAttempts != 3 {
		t.Fatalf("projection topology recycle arrows=%#v", got)
	}
	if got := projection.Runs[0].Topology.Phases; len(got) != 2 ||
		got[0].Name != "env-prep" ||
		got[1].Name != "agent-execute" ||
		len(got[1].DependsOn) != 1 ||
		got[1].DependsOn[0] != "env-prep" {
		t.Fatalf("projection topology phases=%#v", got)
	}
	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "active" || envPhase.Jobs[0].State != "active" || envPhase.Jobs[0].Steps[0].State != "active" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "not_started" || executePhase.Jobs[0].State != "not_started" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
}

func TestWorkflowTopologyForRunMarksRecycleOriginArrowActive(t *testing.T) {
	workflow := Workflow{
		Project: "ambience",
		Name:    "default",
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job"},
			{Name: "llm-work", Kind: "k8s_job", DependsOn: []string{"env-prep"}},
			{
				Name:      "evidence-gate",
				Kind:      "k8s_job",
				DependsOn: []string{"llm-work"},
				RecyclePolicy: &RecyclePolicy{
					MaxAttempts: 3,
					On:          []string{"verify_fail"},
					LandsAt:     "env-prep",
				},
			},
			{
				Name:      "review-surface",
				Kind:      "k8s_job",
				DependsOn: []string{"evidence-gate"},
				RunOn:     PhaseRunOnSuccess,
				Purpose:   PhasePurposeReview,
				Jobs:      []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}},
			},
		},
		PR: PrPrimitive{
			RecyclePolicy: &RecyclePolicy{
				MaxAttempts: 3,
				On:          []string{"changes_requested"},
				LandsAt:     "env-prep",
			},
		},
	}
	origin := "recycle_policy"

	topology := workflowTopologyForRun(workflow, RunReport{
		OriginKind:      &origin,
		EntrypointPhase: stringPtr("env-prep"),
		TriggerSource:   map[string]any{"failing_phase": "evidence-gate"},
	})

	if topology.DefaultEntry == nil || topology.DefaultEntry.Active {
		t.Fatalf("default entry active=%#v, want inactive for recycle", topology.DefaultEntry)
	}
	if len(topology.RecycleArrows) != 2 {
		t.Fatalf("recycle arrows=%#v", topology.RecycleArrows)
	}
	if !topology.RecycleArrows[0].Active || topology.RecycleArrows[1].Active {
		t.Fatalf("recycle arrows active=%#v, want only phase recycle active", topology.RecycleArrows)
	}
}

func TestWorkflowTopologyForRunMarksReviewRecycleOriginArrowActive(t *testing.T) {
	workflow := Workflow{
		Project: "ambience",
		Name:    "default",
		Phases: []PhaseSpec{
			{Name: "env-prep", Kind: "k8s_job"},
			{
				Name:      "evidence-gate",
				Kind:      "k8s_job",
				DependsOn: []string{"env-prep"},
				RecyclePolicy: &RecyclePolicy{
					MaxAttempts: 3,
					On:          []string{"verify_fail"},
					LandsAt:     "env-prep",
				},
			},
			{
				Name:      "review-surface",
				Kind:      "k8s_job",
				DependsOn: []string{"evidence-gate"},
				RunOn:     PhaseRunOnSuccess,
				Purpose:   PhasePurposeReview,
				Jobs:      []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}},
			},
		},
		PR: PrPrimitive{
			RecyclePolicy: &RecyclePolicy{
				MaxAttempts: 3,
				On:          []string{"changes_requested"},
				LandsAt:     "env-prep",
			},
		},
	}
	origin := "pr_feedback"

	topology := workflowTopologyForRun(workflow, RunReport{OriginKind: &origin})

	if topology.DefaultEntry == nil || topology.DefaultEntry.Active {
		t.Fatalf("default entry active=%#v, want inactive for PR feedback recycle", topology.DefaultEntry)
	}
	if len(topology.RecycleArrows) != 2 {
		t.Fatalf("recycle arrows=%#v", topology.RecycleArrows)
	}
	if topology.RecycleArrows[1].Source != "review-surface" {
		t.Fatalf("review recycle source=%q, want registered review phase", topology.RecycleArrows[1].Source)
	}
	if topology.RecycleArrows[0].Active || !topology.RecycleArrows[1].Active {
		t.Fatalf("recycle arrows active=%#v, want only review recycle active", topology.RecycleArrows)
	}
}

func TestWorkflowTopologyForRunKeepsDefaultEntryActiveForDispatch(t *testing.T) {
	workflow := Workflow{
		Project: "ambience",
		Name:    "default",
		Phases:  []PhaseSpec{{Name: "env-prep", Kind: "k8s_job"}},
		PR:      PrPrimitive{},
	}
	origin := "dispatch"

	topology := workflowTopologyForRun(workflow, RunReport{OriginKind: &origin})

	if topology.DefaultEntry == nil || !topology.DefaultEntry.Active {
		t.Fatalf("default entry active=%#v, want active for dispatch", topology.DefaultEntry)
	}
}

func TestRunCycleGraphProjectionUsesDurableExecutions(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	completed := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Jobs: []RunnerJobSpec{{ID: "prepare"}}},
				{Name: "agent-execute", Kind: "k8s_job", DependsOn: []string{"env-prep"}, Jobs: []RunnerJobSpec{{ID: "agent"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-1",
			Project:          "glimmung",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "agent-run",
			IssueRef:         "glimmung#17",
			IssueNumber:      issueNumber,
			State:            "aborted",
			StartedAt:        now,
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{
				{
					Name:        "env-prep",
					Kind:        "k8s_job",
					State:       "failed",
					Reason:      stringPtr("dispatch_timeout"),
					CreatedAt:   now.Format(time.RFC3339Nano),
					CompletedAt: &completed,
					Jobs: []RunJobExecution{{
						ID:          "prepare",
						State:       "failed",
						Reason:      stringPtr("dispatch_timeout"),
						CreatedAt:   now.Format(time.RFC3339Nano),
						CompletedAt: &completed,
						Steps: []RunStepExecution{{
							Slug:      "job",
							State:     "not_started",
							CreatedAt: now.Format(time.RFC3339Nano),
						}},
					}},
				},
				{
					Name:        "agent-execute",
					Kind:        "k8s_job",
					State:       "skipped",
					CreatedAt:   now.Format(time.RFC3339Nano),
					CompletedAt: &completed,
					Jobs: []RunJobExecution{{
						ID:          "agent",
						State:       "skipped",
						CreatedAt:   now.Format(time.RFC3339Nano),
						CompletedAt: &completed,
						Steps: []RunStepExecution{{
							Slug:        "job",
							State:       "skipped",
							CreatedAt:   now.Format(time.RFC3339Nano),
							CompletedAt: &completed,
						}},
					}},
				},
			},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "failed" || envPhase.Reason == nil || *envPhase.Reason != "dispatch_timeout" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].State != "failed" || envPhase.Jobs[0].Reason == nil || *envPhase.Jobs[0].Reason != "dispatch_timeout" {
		t.Fatalf("env-prep job projection=%#v", envPhase.Jobs[0])
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "skipped" || executePhase.Jobs[0].State != "skipped" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
}

func TestProjectionExecutionStateKeepsAbortedStepTerminal(t *testing.T) {
	if got := projectionExecutionState("aborted"); got != "aborted" {
		t.Fatalf("projectionExecutionState(aborted)=%q", got)
	}
	if got := projectionStepState("aborted"); got != "aborted" {
		t.Fatalf("projectionStepState(aborted)=%q", got)
	}
}

func TestRunCycleGraphProjectionShowsCarriedForwardEntrypointInputs(t *testing.T) {
	issueNumber := 171
	runNumber := 4
	cycleNumber := 6
	runCycle := 2
	runDisplay := "4.2"
	now := time.Date(2026, 5, 26, 6, 18, 0, 0, time.UTC)
	completed := now.Format(time.RFC3339Nano)
	completedAt := now
	runRef := "ambience#171/runs/4.2"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "ambience",
			Name:    "default",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Jobs: []RunnerJobSpec{{ID: "env-prep", Name: stringPtr("Environment prep")}}},
				{Name: "llm-work", Kind: "k8s_job", DependsOn: []string{"env-prep"}, Jobs: []RunnerJobSpec{{ID: "llm-implement"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "ambience#171",
			Project: "ambience",
			Number:  &issueNumber,
			Title:   "Bog bubbles",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-4-2",
			Project:          "ambience",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "default",
			IssueRef:         "ambience#171",
			IssueNumber:      issueNumber,
			State:            "review_required",
			StartedAt:        now,
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{{
				Name:        "env-prep",
				Kind:        "k8s_job",
				State:       "skipped",
				CreatedAt:   completed,
				CompletedAt: &completed,
				Jobs: []RunJobExecution{{
					ID:          "env-prep",
					State:       "skipped",
					CreatedAt:   completed,
					CompletedAt: &completed,
					Steps: []RunStepExecution{{
						Slug:        "emit-env-outputs",
						State:       "skipped",
						CreatedAt:   completed,
						CompletedAt: &completed,
					}},
				}},
			}},
			Attempts: []RunReportAttempt{{
				AttemptIndex: 0,
				Phase:        "env-prep",
				PhaseKind:    "k8s_job",
				CarryForward: true,
				DispatchedAt: now,
				CompletedAt:  &completedAt,
				Conclusion:   stringPtr("success"),
				Decision:     stringPtr("advance"),
				PhaseOutputs: map[string]string{"validation_url": "https://slot.example"},
				JobCompletions: []RunAttemptJobCompletion{{
					JobID:       "env-prep",
					CompletedAt: &completedAt,
					Conclusion:  "success",
					CostUSD:     0.25,
				}},
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/ambience/issues/171/runs/4/cycles/2/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "succeeded" || envPhase.Reason == nil || *envPhase.Reason != "carried_forward" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].State != "succeeded" || envPhase.Jobs[0].Reason == nil || *envPhase.Jobs[0].Reason != "carried_forward" {
		t.Fatalf("env-prep job projection=%#v", envPhase.Jobs[0])
	}
	if envPhase.Jobs[0].CostUSD == nil || *envPhase.Jobs[0].CostUSD != 0.25 {
		t.Fatalf("env-prep job cost=%#v", envPhase.Jobs[0].CostUSD)
	}
	if !envPhase.Attempts[0].CarryForward {
		t.Fatalf("carry_forward not projected: %#v", envPhase.Attempts[0])
	}
}

func TestRunCycleGraphProjectionKeepsPendingWorkflowJobsWithDurableExecutions(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{
					Name: "env-prep",
					Kind: "k8s_job",
					Jobs: []RunnerJobSpec{{
						ID: "prepare",
						Steps: []RunnerStepSpec{{
							Slug:  "checkout",
							Title: stringPtr("Checkout"),
						}},
					}},
				},
				{
					Name:      "agent-execute",
					Kind:      "k8s_job",
					DependsOn: []string{"env-prep"},
					Jobs: []RunnerJobSpec{{
						ID:   "agent",
						Name: stringPtr("Run agent"),
						Steps: []RunnerStepSpec{{
							Slug:       "run-agent",
							Title:      stringPtr("Run agent"),
							Group:      "sweep-01",
							GroupTitle: stringPtr("sweep 01"),
						}},
					}},
				},
			},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-1",
			Project:          "glimmung",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "agent-run",
			IssueRef:         "glimmung#17",
			IssueNumber:      issueNumber,
			State:            "in_progress",
			CurrentPhase:     stringPtr("env-prep"),
			StartedAt:        now,
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{{
				Name:      "env-prep",
				Kind:      "k8s_job",
				State:     "active",
				CreatedAt: now.Format(time.RFC3339Nano),
				Jobs: []RunJobExecution{{
					ID:        "prepare",
					State:     "active",
					CreatedAt: now.Format(time.RFC3339Nano),
					StartedAt: stringPtr(now.Add(-2 * time.Minute).Format(time.RFC3339Nano)),
					Steps: []RunStepExecution{{
						Slug:       "checkout",
						Title:      stringPtr("Checkout"),
						State:      "active",
						Group:      "setup",
						GroupTitle: stringPtr("setup"),
						CreatedAt:  now.Format(time.RFC3339Nano),
						StartedAt:  stringPtr(now.Add(-90 * time.Second).Format(time.RFC3339Nano)),
					}},
				}},
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "active" || envPhase.Jobs[0].Steps[0].State != "active" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].StartedAt == nil || *envPhase.Jobs[0].StartedAt != now.Add(-2*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("env-prep job started_at=%v", envPhase.Jobs[0].StartedAt)
	}
	if envPhase.Jobs[0].Steps[0].StartedAt == nil || *envPhase.Jobs[0].Steps[0].StartedAt != now.Add(-90*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("env-prep step started_at=%v", envPhase.Jobs[0].Steps[0].StartedAt)
	}
	if envPhase.Jobs[0].Steps[0].Group != "setup" || envPhase.Jobs[0].Steps[0].GroupTitle == nil || *envPhase.Jobs[0].Steps[0].GroupTitle != "setup" {
		t.Fatalf("env-prep step group=%#v", envPhase.Jobs[0].Steps[0])
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "not_started" || len(executePhase.Jobs) != 1 || executePhase.Jobs[0].State != "not_started" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
	if len(executePhase.Jobs[0].Steps) != 1 ||
		executePhase.Jobs[0].Steps[0].Slug != "run-agent" ||
		executePhase.Jobs[0].Steps[0].State != "not_started" ||
		executePhase.Jobs[0].Steps[0].Group != "sweep-01" ||
		executePhase.Jobs[0].Steps[0].GroupTitle == nil ||
		*executePhase.Jobs[0].Steps[0].GroupTitle != "sweep 01" {
		t.Fatalf("agent-execute steps=%#v", executePhase.Jobs[0].Steps)
	}
}

func TestApplyNativeEventsResetsUnobservedFailedSteps(t *testing.T) {
	exitCode := 1
	stepFailed := "step_failed"
	exitNonzero := "exit_nonzero"
	jobFailed := "job_failed"
	run := RunProjectionRun{
		Phases: []RunProjectionPhase{{
			Name: "env-prep",
			Kind: "k8s_job",
			Attempts: []RunProjectionAttempt{{
				AttemptIndex: 0,
				Phase:        "env-prep",
				PhaseKind:    "k8s_job",
			}},
			Jobs: []RunProjectionJob{{
				ID:     "env-prep",
				State:  "failed",
				Reason: &stepFailed,
				Steps: []RunProjectionStep{
					{Slug: "check-validation-env", State: "failed", Reason: &exitNonzero, ExitCode: &exitCode},
					{Slug: "emit-env-outputs", State: "failed", Reason: &jobFailed},
				},
			}},
		}},
	}
	events := []RunnerLogEvent{{
		AttemptIndex: 0,
		Phase:        "env-prep",
		JobID:        "env-prep",
		Seq:          173,
		Event:        "step_failed",
		StepSlug:     "check-validation-env",
		ExitCode:     &exitCode,
	}}

	applyRunnerEventsToProjectionRun(&run, events)

	steps := run.Phases[0].Jobs[0].Steps
	if steps[0].State != "failed" || steps[0].Reason == nil || *steps[0].Reason != "exit_nonzero" {
		t.Fatalf("check-validation-env step=%#v", steps[0])
	}
	if steps[1].State != "not_started" || steps[1].Reason != nil || steps[1].ExitCode != nil {
		t.Fatalf("emit-env-outputs step=%#v", steps[1])
	}
}

func TestApplyNativeEventsProjectsDynamicGroupConcreteSteps(t *testing.T) {
	run := RunProjectionRun{
		Phases: []RunProjectionPhase{{
			Name: "llm-verify",
			Kind: "k8s_job",
			Attempts: []RunProjectionAttempt{{
				AttemptIndex: 0,
				Phase:        "llm-verify",
				PhaseKind:    "k8s_job",
			}},
			Jobs: []RunProjectionJob{{
				ID:    "verify",
				State: "active",
				Steps: []RunProjectionStep{
					{Slug: "author-test-plan", State: "succeeded"},
					{Slug: "gather-evidence", State: "not_started", Group: "test-cases", GroupTitle: stringPtr("Test cases generated at runtime"), DynamicGroup: &StepDynamicGroup{MaxItems: 10, ItemLabel: "test case"}},
					{Slug: "judge-evidence", State: "not_started", Group: "test-cases", GroupTitle: stringPtr("Test cases generated at runtime"), DynamicGroup: &StepDynamicGroup{MaxItems: 10, ItemLabel: "test case"}},
				},
			}},
		}},
	}
	events := []RunnerLogEvent{
		{
			AttemptIndex: 0,
			Phase:        "llm-verify",
			JobID:        "verify",
			Seq:          10,
			Event:        "dynamic_group_expanded",
			Metadata: map[string]any{
				"group":       "test-cases",
				"group_title": "Test cases generated at runtime",
				"item_count":  float64(1),
				"steps": []any{
					map[string]any{"slug": "gather-evidence-case-01", "title": "gather-evidence-case-01", "group": "test-cases/case-01", "group_title": "home page"},
					map[string]any{"slug": "judge-evidence-case-01", "title": "judge-evidence-case-01", "group": "test-cases/case-01", "group_title": "home page"},
				},
			},
		},
		{
			AttemptIndex: 0,
			Phase:        "llm-verify",
			JobID:        "verify",
			Seq:          11,
			Event:        "step_started",
			StepSlug:     "gather-evidence-case-01",
			Metadata: map[string]any{
				"group":       "test-cases/case-01",
				"group_title": "home page",
			},
		},
	}

	applyRunnerEventsToProjectionRun(&run, events)

	steps := run.Phases[0].Jobs[0].Steps
	if len(steps) != 3 || steps[1].Slug != "gather-evidence-case-01" || steps[2].Slug != "judge-evidence-case-01" {
		t.Fatalf("steps=%#v", steps)
	}
	if steps[1].State != "active" || steps[1].Group != "test-cases/case-01" || steps[1].GroupTitle == nil || *steps[1].GroupTitle != "home page" {
		t.Fatalf("concrete step=%#v", steps[1])
	}
	if steps[2].State != "not_started" || steps[2].Group != "test-cases/case-01" || steps[2].GroupTitle == nil || *steps[2].GroupTitle != "home page" {
		t.Fatalf("planned concrete step=%#v", steps[2])
	}
}

func TestRunCycleGraphProjectionShowsLegacyAbortedDispatchTimeout(t *testing.T) {
	issueNumber := 17
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	runRef := "glimmung#17/runs/1.1"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "glimmung",
			Name:    "agent-run",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Jobs: []RunnerJobSpec{{ID: "prepare"}}},
				{Name: "agent-execute", Kind: "k8s_job", DependsOn: []string{"env-prep"}, Jobs: []RunnerJobSpec{{ID: "agent"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &issueNumber,
			Title:   "Port graph",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-1",
			Project:          "glimmung",
			RunRef:           runRef,
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "agent-run",
			IssueRef:         "glimmung#17",
			IssueNumber:      issueNumber,
			State:            "aborted",
			CurrentPhase:     stringPtr("env-prep"),
			AbortReason:      stringPtr("dispatch_timeout"),
			StartedAt:        now,
			UpdatedAt:        now,
			Attempts: []RunReportAttempt{{
				AttemptIndex:     0,
				Phase:            "env-prep",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:env-prep",
				DispatchedAt:     now.Add(-11 * time.Minute),
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/glimmung/issues/17/runs/1/cycles/1/graph", &projection)

	envPhase := assertProjectionPhase(t, projection.Runs[0], "env-prep")
	if envPhase.State != "failed" || envPhase.Reason == nil || *envPhase.Reason != "dispatch_timeout" {
		t.Fatalf("env-prep projection=%#v", envPhase)
	}
	if envPhase.Jobs[0].State != "failed" || envPhase.Jobs[0].Reason == nil || *envPhase.Jobs[0].Reason != "dispatch_timeout" {
		t.Fatalf("env-prep job projection=%#v", envPhase.Jobs[0])
	}
	executePhase := assertProjectionPhase(t, projection.Runs[0], "agent-execute")
	if executePhase.State != "skipped" || executePhase.Jobs[0].State != "skipped" {
		t.Fatalf("agent-execute projection=%#v", executePhase)
	}
}

func TestRunCycleGraphProjectionShowsForwardDispatchFailureWithDispatchStepOwnership(t *testing.T) {
	issueNumber := 171
	runNumber := 3
	cycleNumber := 4
	runCycle := 1
	runDisplay := "3.1"
	now := time.Date(2026, 5, 25, 7, 32, 14, 0, time.UTC)
	abortReason := `forward_dispatch_failed: phase "llm-work" input "claude_ca_namespace" refs phase "env-prep" which has no captured outputs on this run`
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "ambience",
			Name:    "default",
			Phases: []PhaseSpec{
				{Name: "env-prep", Kind: "k8s_job", Outputs: []string{"claude_ca_namespace"}, Jobs: []RunnerJobSpec{{ID: "env-prep"}}},
				{
					Name:      "llm-work",
					Kind:      "k8s_job",
					DependsOn: []string{"env-prep"},
					Inputs:    map[string]string{"claude_ca_namespace": "${{ phases.env-prep.outputs.claude_ca_namespace }}"},
					Jobs: []RunnerJobSpec{
						{ID: "llm-test-plan", Steps: []RunnerStepSpec{{Slug: "clone"}, {Slug: "run-test-plan"}}},
						{ID: "llm-implement", Steps: []RunnerStepSpec{{Slug: "clone"}, {Slug: "run-implementation"}}},
					},
				},
				{Name: "llm-verify", Kind: "k8s_job", DependsOn: []string{"llm-work"}, Jobs: []RunnerJobSpec{{ID: "llm-verify"}}},
			},
		}}},
		issue: IssueDetail{
			Ref:     "ambience#171",
			Project: "ambience",
			Number:  &issueNumber,
			Title:   "Bog bubbles",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-3",
			Project:          "ambience",
			RunRef:           "ambience#171/runs/3.1",
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "default",
			IssueRef:         "ambience#171",
			IssueNumber:      issueNumber,
			State:            "aborted",
			CurrentPhase:     stringPtr("llm-work"),
			AbortReason:      &abortReason,
			StartedAt:        now.Add(-time.Minute),
			UpdatedAt:        now,
			Attempts: []RunReportAttempt{{
				AttemptIndex:     0,
				Phase:            "env-prep",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:env-prep",
				DispatchedAt:     now.Add(-time.Minute),
				CompletedAt:      &now,
				Conclusion:       stringPtr("success"),
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/ambience/issues/171/runs/3/cycles/1/graph", &projection)

	if projection.Runs[0].AbortReason == nil || *projection.Runs[0].AbortReason != abortReason {
		t.Fatalf("run abort reason not projected: %#v", projection.Runs[0].AbortReason)
	}
	workPhase := assertProjectionPhase(t, projection.Runs[0], "llm-work")
	if workPhase.State != "failed" || workPhase.Reason == nil || *workPhase.Reason != "dispatch_failed" {
		t.Fatalf("llm-work projection=%#v", workPhase)
	}
	for _, job := range workPhase.Jobs {
		if job.State != "failed" || job.Reason == nil || *job.Reason != "dispatch_failed" {
			t.Fatalf("llm-work job projection=%#v", job)
		}
		if len(job.Steps) == 0 || job.Steps[0].Slug != "dispatch" || job.Steps[0].State != "failed" ||
			job.Steps[0].Reason == nil || *job.Steps[0].Reason != "dispatch_failed" {
			t.Fatalf("dispatch failure should be owned by a dispatch step: %#v", job.Steps)
		}
		for _, step := range job.Steps[1:] {
			if step.State != "not_started" || step.Reason != nil || step.ExitCode != nil {
				t.Fatalf("workflow step should remain unstarted under dispatch failure: %#v", step)
			}
		}
	}
}

func TestRunCycleGraphProjectionPromotesSkippedRowsForDispatchFailureOwnership(t *testing.T) {
	issueNumber := 168
	runNumber := 5
	cycleNumber := 8
	runCycle := 2
	runDisplay := "5.2"
	now := time.Date(2026, 6, 1, 7, 19, 41, 0, time.UTC)
	abortReason := `forward_dispatch_failed: runner lease state is "released" for ambience-slot-3, want claimed; cleanup_dispatch_failed: runner lease state is "released" for ambience-slot-3, want claimed`
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "ambience",
			Name:    "default",
			Phases: []PhaseSpec{
				{Name: "prepare", Kind: "k8s_job", Jobs: []RunnerJobSpec{{ID: "env-prep"}}},
				{Name: "llm-work", Kind: "k8s_job", DependsOn: []string{"prepare"}, Jobs: []RunnerJobSpec{{ID: "llm-implement"}}},
				{
					Name:      "llm-verify",
					Kind:      "k8s_job",
					DependsOn: []string{"llm-work"},
					Verify:    true,
					Jobs: []RunnerJobSpec{{
						ID: "llm-verify",
						Steps: []RunnerStepSpec{
							{Slug: "clone", Title: stringPtr("Clone repo")},
							{Slug: "run-verification", Title: stringPtr("Run verification")},
						},
					}},
				},
			},
		}}},
		issue: IssueDetail{
			Ref:     "ambience#168",
			Project: "ambience",
			Number:  &issueNumber,
			Title:   "Magic portal",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-5",
			Project:          "ambience",
			RunRef:           "ambience#168/runs/5.2",
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "default",
			IssueRef:         "ambience#168",
			IssueNumber:      issueNumber,
			State:            "aborted",
			CurrentPhase:     stringPtr("llm-verify"),
			AbortReason:      &abortReason,
			StartedAt:        now.Add(-20 * time.Minute),
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{{
				Name:      "llm-verify",
				Kind:      "k8s_job",
				State:     "failed",
				Reason:    stringPtr("dispatch_failed"),
				CreatedAt: now.Add(-20 * time.Minute).Format(time.RFC3339Nano),
				Jobs: []RunJobExecution{{
					ID:          "llm-verify",
					Name:        stringPtr("LLM: Run tests"),
					State:       "skipped",
					CompletedAt: stringPtr(now.Add(-11 * time.Minute).Format(time.RFC3339Nano)),
					Steps: []RunStepExecution{
						{Slug: "clone", Title: stringPtr("Clone repo"), State: "skipped"},
						{Slug: "run-verification", Title: stringPtr("Run verification"), State: "skipped"},
					},
				}},
			}},
			Attempts: []RunReportAttempt{{
				AttemptIndex:     2,
				Phase:            "llm-verify",
				PhaseKind:        "k8s_job",
				WorkflowFilename: "k8s_job:llm-verify",
				DispatchedAt:     now.Add(-time.Second),
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/ambience/issues/168/runs/5/cycles/2/graph", &projection)

	verifyPhase := assertProjectionPhase(t, projection.Runs[0], "llm-verify")
	if verifyPhase.State != "failed" || verifyPhase.Reason == nil || *verifyPhase.Reason != "dispatch_failed" {
		t.Fatalf("llm-verify projection=%#v", verifyPhase)
	}
	job := verifyPhase.Jobs[0]
	if job.State != "failed" || job.Reason == nil || *job.Reason != "dispatch_failed" {
		t.Fatalf("llm-verify job projection=%#v", job)
	}
	if len(job.Steps) != 3 || job.Steps[0].Slug != "dispatch" || job.Steps[0].State != "failed" {
		t.Fatalf("llm-verify dispatch ownership steps=%#v", job.Steps)
	}
	for _, step := range job.Steps[1:] {
		if step.State != "not_started" || step.Reason != nil || step.ExitCode != nil {
			t.Fatalf("workflow step should not own dispatch failure: %#v", step)
		}
	}
}

// assertFailedJobsOwnAFailedStep is the general run-graph owner-step
// completeness invariant: no job projected in a failed/aborted state may render
// with every step succeeded, skipped, or not_started — at least one `failed`
// owner step must carry the failure.
func assertFailedJobsOwnAFailedStep(t *testing.T, run RunProjectionRun) {
	t.Helper()
	for _, phase := range run.Phases {
		for _, job := range phase.Jobs {
			if job.State != "failed" && job.State != "aborted" {
				continue
			}
			ownerFailed := false
			for _, step := range job.Steps {
				if step.State == "failed" {
					ownerFailed = true
					break
				}
			}
			if !ownerFailed {
				encoded, _ := json.MarshalIndent(job, "", "  ")
				t.Fatalf("failed job %q/%q projected with no failed owner step: %s", phase.Name, job.ID, encoded)
			}
		}
	}
}

func TestEnsureFailedJobOwnerStepShapes(t *testing.T) {
	verificationFailed := "verification_failed"
	dispatchFailed := "dispatch_failed"
	jobFailed := "job_failed"
	exitNonzero := "exit_nonzero"
	exitCode := 1
	greenSteps := func() []RunProjectionStep {
		return []RunProjectionStep{
			{Slug: "build-and-deploy", State: "succeeded"},
			{Slug: "prepare-scenario", State: "succeeded"},
			{Slug: "run-verification", State: "succeeded"},
			{Slug: "collect-evidence", State: "succeeded"},
			{Slug: "finalize-verification", State: "succeeded"},
		}
	}

	t.Run("verdict failure appends a failed owner step and keeps real steps", func(t *testing.T) {
		detail := "verifier reported status=abort reason=claimed_result_not_observed"
		out := ensureFailedJobOwnerStep("failed", &verificationFailed, &detail, greenSteps(), false)
		if len(out) != 6 {
			t.Fatalf("expected the five real steps plus a verdict owner, got %#v", out)
		}
		for _, step := range out[:5] {
			if step.State != "succeeded" {
				t.Fatalf("real step demoted by verdict synthesis: %#v", step)
			}
		}
		owner := out[5]
		if owner.Slug != "verdict" || owner.State != "failed" || owner.Reason == nil || *owner.Reason != "verification_failed" {
			t.Fatalf("verdict owner step=%#v", owner)
		}
		// The synthesized verdict step must carry the deciding failure detail as
		// its message, not just the bare reason enum — this is what stops the
		// step pane from rendering "No hot runner events recorded".
		if owner.Message == nil || *owner.Message != detail {
			t.Fatalf("verdict owner step must carry the failure detail message: %#v", owner)
		}
	})

	t.Run("producer job_failed verdict still gets a reason-carrying owner", func(t *testing.T) {
		out := ensureFailedJobOwnerStep("failed", &jobFailed, nil, []RunProjectionStep{{Slug: "produce", State: "succeeded"}}, false)
		owner := out[len(out)-1]
		if owner.Slug != "verdict" || owner.State != "failed" || owner.Reason == nil || *owner.Reason != "job_failed" {
			t.Fatalf("producer verdict owner=%#v", owner)
		}
		// No completion detail available (producer job_failed with no reasons):
		// the owner keeps its reason enum and carries no message rather than
		// inventing one.
		if owner.Message != nil {
			t.Fatalf("owner with no detail must not fabricate a message: %#v", owner)
		}
	})

	t.Run("never-ran dispatch synthesizes dispatch and demotes unstarted steps", func(t *testing.T) {
		steps := []RunProjectionStep{
			{Slug: "clone", State: "skipped"},
			{Slug: "run-verification", State: "skipped"},
		}
		out := ensureFailedJobOwnerStep("failed", &dispatchFailed, nil, steps, true)
		if len(out) != 3 || out[0].Slug != "dispatch" || out[0].State != "failed" || out[0].Reason == nil || *out[0].Reason != "dispatch_failed" {
			t.Fatalf("dispatch owner steps=%#v", out)
		}
		for _, step := range out[1:] {
			if step.State != "not_started" || step.Reason != nil || step.ExitCode != nil {
				t.Fatalf("never-ran step should be demoted to not_started: %#v", step)
			}
		}
	})

	t.Run("dispatch reason but completion present is a verdict, not a demotion", func(t *testing.T) {
		// neverRan=false: the steps ran, so we must not demote them even though
		// the reason looks dispatch-shaped.
		out := ensureFailedJobOwnerStep("failed", &dispatchFailed, nil, greenSteps(), false)
		if out[len(out)-1].Slug != "verdict" {
			t.Fatalf("expected verdict owner when steps ran: %#v", out)
		}
		for _, step := range out[:5] {
			if step.State != "succeeded" {
				t.Fatalf("ran step demoted: %#v", step)
			}
		}
	})

	t.Run("dynamic_step_group failing case is already owned and left untouched", func(t *testing.T) {
		steps := []RunProjectionStep{
			{Slug: "gather-evidence-case-01", State: "succeeded"},
			{Slug: "judge-evidence-case-01", State: "failed", Reason: &exitNonzero, ExitCode: &exitCode},
			{Slug: "aggregate-verification", State: "not_started"},
		}
		out := ensureFailedJobOwnerStep("failed", &verificationFailed, nil, steps, false)
		if len(out) != 3 {
			t.Fatalf("already-owned job must not be double-synthesized: %#v", out)
		}
		if out[1].Slug != "judge-evidence-case-01" || out[1].State != "failed" {
			t.Fatalf("real failed case step mutated: %#v", out[1])
		}
	})

	t.Run("aborted job is also covered", func(t *testing.T) {
		out := ensureFailedJobOwnerStep("aborted", &verificationFailed, nil, greenSteps(), false)
		if out[len(out)-1].State != "failed" {
			t.Fatalf("aborted job should own a failed step: %#v", out)
		}
	})

	t.Run("non-terminal job is never given a synthetic failure", func(t *testing.T) {
		for _, state := range []string{"succeeded", "active", "skipped", "not_started"} {
			out := ensureFailedJobOwnerStep(state, &verificationFailed, nil, greenSteps(), false)
			if len(out) != 5 {
				t.Fatalf("state %q should be untouched: %#v", state, out)
			}
		}
	})
}

func TestRunCycleGraphProjectionOwnsVerifierVerdictFailure(t *testing.T) {
	// spirelens#147 1.1: the verify job projected failed (verification_failed)
	// but every step projected succeeded. The failed job must carry a `failed`
	// owner step with the reason, and the real (succeeded) steps must NOT be
	// demoted to not_started — they genuinely ran.
	issueNumber := 147
	runNumber := 1
	cycleNumber := 1
	runCycle := 1
	runDisplay := "1.1"
	now := time.Date(2026, 6, 14, 9, 30, 0, 0, time.UTC)
	verificationFailed := "fail"
	store := fakeGraphStore{
		fakeReadStore: fakeReadStore{workflows: []Workflow{{
			Project: "spirelens",
			Name:    "default",
			Phases: []PhaseSpec{
				{Name: "prepare", Kind: "k8s_job", Jobs: []RunnerJobSpec{{ID: "env-prep"}}},
				{
					Name:      "testing",
					Kind:      "k8s_job",
					DependsOn: []string{"prepare"},
					Verify:    true,
					Jobs: []RunnerJobSpec{{
						ID: "verify",
						Steps: []RunnerStepSpec{
							{Slug: "build-and-deploy", Title: stringPtr("Build and deploy")},
							{Slug: "prepare-scenario", Title: stringPtr("Prepare scenario")},
							{Slug: "run-verification", Title: stringPtr("Run verification")},
							{Slug: "collect-evidence", Title: stringPtr("Collect evidence")},
							{Slug: "finalize-verification", Title: stringPtr("Finalize verification")},
						},
					}},
				},
			},
		}}},
		issue: IssueDetail{
			Ref:     "spirelens#147",
			Project: "spirelens",
			Number:  &issueNumber,
			Title:   "Verify run",
			State:   "open",
		},
		runs: []RunReport{{
			ID:               "run-147",
			Project:          "spirelens",
			RunRef:           "spirelens#147/runs/1.1",
			RunNumber:        &runNumber,
			CycleNumber:      &cycleNumber,
			RunCycleNumber:   &runCycle,
			RunDisplayNumber: &runDisplay,
			Workflow:         "default",
			IssueRef:         "spirelens#147",
			IssueNumber:      issueNumber,
			State:            "aborted",
			CurrentPhase:     stringPtr("testing"),
			StartedAt:        now.Add(-30 * time.Minute),
			UpdatedAt:        now,
			PhaseExecutions: []RunPhaseExecution{{
				Name:      "testing",
				Kind:      "k8s_job",
				State:     "failed",
				Reason:    stringPtr("verification_failed"),
				CreatedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
				Jobs: []RunJobExecution{{
					ID:        "verify",
					Name:      stringPtr("Verify"),
					State:     "failed",
					Reason:    stringPtr("verification_failed"),
					CreatedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
					Steps: []RunStepExecution{
						{Slug: "build-and-deploy", State: "succeeded"},
						{Slug: "prepare-scenario", State: "succeeded"},
						{Slug: "run-verification", State: "succeeded"},
						{Slug: "collect-evidence", State: "succeeded"},
						{Slug: "finalize-verification", State: "succeeded"},
					},
				}},
			}},
			Attempts: []RunReportAttempt{{
				AttemptIndex:       0,
				Phase:              "testing",
				PhaseKind:          "k8s_job",
				WorkflowFilename:   "k8s_job:testing",
				DispatchedAt:       now.Add(-30 * time.Minute),
				CompletedAt:        &now,
				Conclusion:         stringPtr("failure"),
				VerificationStatus: &verificationFailed,
				SummaryMarkdown:    stringPtr("The Booming Conch tooltip shows only the stock game tooltip; the 'additional cards drawn: 2' stat row is absent."),
				JobCompletions: []RunAttemptJobCompletion{{
					JobID:               "verify",
					Conclusion:          "failure",
					VerificationStatus:  &verificationFailed,
					VerificationReasons: []string{"verifier reported status=abort reason=claimed_result_not_observed"},
					CompletedAt:         &now,
				}},
			}},
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var projection RunGraphProjection
	getJSON(t, handler, "/v1/projects/spirelens/issues/147/runs/1/cycles/1/graph", &projection)

	assertFailedJobsOwnAFailedStep(t, projection.Runs[0])

	testingPhase := assertProjectionPhase(t, projection.Runs[0], "testing")
	if testingPhase.State != "failed" {
		t.Fatalf("testing phase=%#v", testingPhase)
	}
	job := testingPhase.Jobs[0]
	if job.State != "failed" || job.Reason == nil || *job.Reason != "verification_failed" {
		t.Fatalf("verify job=%#v", job)
	}
	realSlugs := map[string]bool{
		"build-and-deploy": true, "prepare-scenario": true, "run-verification": true,
		"collect-evidence": true, "finalize-verification": true,
	}
	var verdictSteps int
	for _, step := range job.Steps {
		if realSlugs[step.Slug] {
			if step.State != "succeeded" {
				t.Fatalf("real verify step was demoted, that would be a new lie: %#v", step)
			}
			continue
		}
		if step.State == "failed" {
			verdictSteps++
			if step.Reason == nil || *step.Reason != "verification_failed" {
				t.Fatalf("verdict owner step must carry the reason: %#v", step)
			}
			// The verdict step must also carry the deciding verifier reason as
			// its message — the projection layer is the source of truth, so a
			// downstream consumer (dashboard step pane, MCP step view) never
			// sees a bare enum next to an empty event stream.
			wantMsg := "verifier reported status=abort reason=claimed_result_not_observed"
			if step.Message == nil || *step.Message != wantMsg {
				t.Fatalf("verdict owner step must carry the verifier reason as message, got %#v", step)
			}
		}
	}
	if verdictSteps != 1 {
		t.Fatalf("expected exactly one synthetic verdict owner step, got %d in %#v", verdictSteps, job.Steps)
	}

	// The deciding attempt's summary prose must survive into the projection —
	// it is dropped pre-fix, which is why no frontend-only change could surface
	// the rich "why" on the verdict view.
	attempt := testingPhase.Attempts[len(testingPhase.Attempts)-1]
	if attempt.SummaryMarkdown == nil || !strings.Contains(*attempt.SummaryMarkdown, "additional cards drawn: 2") {
		t.Fatalf("projection attempt must carry summary_markdown, got %#v", attempt.SummaryMarkdown)
	}
}

func TestApplyNativeEventsOwnsGreenedVerifierVerdictFailure(t *testing.T) {
	// The runner-event overlay greens steps from step_completed events. A
	// verdict-failed verify job (no step_failed event) would otherwise end up a
	// failed job with every step succeeded. Post-overlay enforcement must add a
	// verdict owner without demoting the greened steps.
	verificationFailed := "verification_failed"
	exit0 := 0
	run := RunProjectionRun{
		Phases: []RunProjectionPhase{{
			Name:   "testing",
			Kind:   "k8s_job",
			State:  "failed",
			Reason: &verificationFailed,
			Attempts: []RunProjectionAttempt{{
				AttemptIndex: 0,
				Phase:        "testing",
				PhaseKind:    "k8s_job",
			}},
			Jobs: []RunProjectionJob{{
				ID:     "verify",
				State:  "failed",
				Reason: &verificationFailed,
				Steps: []RunProjectionStep{
					{Slug: "run-verification", State: "failed"},
					{Slug: "finalize-verification", State: "failed"},
				},
			}},
		}},
	}
	events := []RunnerLogEvent{
		{AttemptIndex: 0, Phase: "testing", JobID: "verify", Seq: 1, Event: "step_completed", StepSlug: "run-verification", ExitCode: &exit0},
		{AttemptIndex: 0, Phase: "testing", JobID: "verify", Seq: 2, Event: "step_completed", StepSlug: "finalize-verification", ExitCode: &exit0},
	}

	applyRunnerEventsToProjectionRun(&run, events)

	assertFailedJobsOwnAFailedStep(t, run)

	steps := run.Phases[0].Jobs[0].Steps
	if len(steps) != 3 {
		t.Fatalf("expected greened steps plus a verdict owner, got %#v", steps)
	}
	if steps[0].State != "succeeded" || steps[1].State != "succeeded" {
		t.Fatalf("greened steps should remain succeeded: %#v", steps[:2])
	}
	verdict := steps[2]
	if verdict.Slug != "verdict" || verdict.State != "failed" || verdict.Reason == nil || *verdict.Reason != "verification_failed" {
		t.Fatalf("verdict owner step=%#v", verdict)
	}
}

func TestApplyNativeEventsLeavesDynamicFailedCaseSingleOwner(t *testing.T) {
	// dynamic_step_group: the runner marks the failing case step failed itself.
	// The invariant is already satisfied; enforcement must not double-synthesize
	// a verdict owner.
	verificationFailed := "verification_failed"
	exit0 := 0
	exit1 := 1
	run := RunProjectionRun{
		Phases: []RunProjectionPhase{{
			Name:   "testing",
			Kind:   "k8s_job",
			State:  "failed",
			Reason: &verificationFailed,
			Attempts: []RunProjectionAttempt{{
				AttemptIndex: 0,
				Phase:        "testing",
				PhaseKind:    "k8s_job",
			}},
			Jobs: []RunProjectionJob{{
				ID:     "verify",
				State:  "failed",
				Reason: &verificationFailed,
				Steps: []RunProjectionStep{
					{Slug: "gather-evidence-case-01", State: "not_started"},
					{Slug: "judge-evidence-case-01", State: "not_started"},
				},
			}},
		}},
	}
	events := []RunnerLogEvent{
		{AttemptIndex: 0, Phase: "testing", JobID: "verify", Seq: 1, Event: "step_completed", StepSlug: "gather-evidence-case-01", ExitCode: &exit0},
		{AttemptIndex: 0, Phase: "testing", JobID: "verify", Seq: 2, Event: "step_failed", StepSlug: "judge-evidence-case-01", ExitCode: &exit1},
	}

	applyRunnerEventsToProjectionRun(&run, events)

	assertFailedJobsOwnAFailedStep(t, run)

	steps := run.Phases[0].Jobs[0].Steps
	if len(steps) != 2 {
		t.Fatalf("dynamic failing case must not be double-synthesized: %#v", steps)
	}
	failedCount := 0
	for _, step := range steps {
		if step.Slug == "verdict" {
			t.Fatalf("synthetic verdict added despite a real failed case step: %#v", steps)
		}
		if step.State == "failed" {
			failedCount++
		}
	}
	if failedCount != 1 || steps[1].Slug != "judge-evidence-case-01" || steps[1].State != "failed" {
		t.Fatalf("expected the real failing case to own the failure: %#v", steps)
	}
}

func TestSystemGraphUsesProjectFilter(t *testing.T) {
	number := 17
	now := time.Date(2026, 5, 12, 18, 0, 0, 0, time.UTC)
	store := fakeGraphStore{
		issues: []IssueRow{{
			Ref:     "glimmung#17",
			Project: "glimmung",
			Number:  &number,
			Title:   "Port graph",
			State:   "open",
		}},
		runs: []RunReport{{
			Project:     "glimmung",
			RunRef:      "glimmung#17/runs/1",
			Workflow:    "agent-run",
			IssueRef:    "glimmung#17",
			IssueNumber: number,
			State:       "in_progress",
			StartedAt:   now,
			UpdatedAt:   now,
		}},
	}
	handler := NewWithStore(Settings{}, store)

	var graph IssueGraph
	getJSON(t, handler, "/v1/graph?project=glimmung", &graph)

	assertGraphNode(t, graph, "issue:glimmung#17", "issue")
	assertGraphNode(t, graph, "run:glimmung#17/runs/1", "run")
	assertGraphEdge(t, graph, "issue:glimmung#17", "run:glimmung#17/runs/1", "spawned")
}

func assertGraphNode(t *testing.T, graph IssueGraph, id, kind string) GraphNode {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.ID == id {
			if node.Kind != kind {
				t.Fatalf("node %s kind=%s, want %s", id, node.Kind, kind)
			}
			return node
		}
	}
	encoded, _ := json.MarshalIndent(graph.Nodes, "", "  ")
	t.Fatalf("missing node %s in %s", id, encoded)
	return GraphNode{}
}

func assertGraphEdge(t *testing.T, graph IssueGraph, source, target, kind string) {
	t.Helper()
	for _, edge := range graph.Edges {
		if edge.Source == source && edge.Target == target && edge.Kind == kind {
			return
		}
	}
	encoded, _ := json.MarshalIndent(graph.Edges, "", "  ")
	t.Fatalf("missing edge %s --%s--> %s in %s", source, kind, target, encoded)
}

func assertProjectionPhase(t *testing.T, run RunProjectionRun, name string) RunProjectionPhase {
	t.Helper()
	for _, phase := range run.Phases {
		if phase.Name == name {
			return phase
		}
	}
	encoded, _ := json.MarshalIndent(run.Phases, "", "  ")
	t.Fatalf("missing projection phase %s in %s", name, encoded)
	return RunProjectionPhase{}
}

func assertProjectionEvidence(t *testing.T, run RunProjectionRun, kind, ref string) {
	t.Helper()
	_ = findProjectionEvidence(t, run, kind, ref)
}

func findProjectionEvidence(t *testing.T, run RunProjectionRun, kind, ref string) RunProjectionEvidence {
	t.Helper()
	for _, evidence := range run.Evidence {
		if evidence.Kind == kind && evidence.Ref == ref {
			return evidence
		}
	}
	encoded, _ := json.MarshalIndent(run.Evidence, "", "  ")
	t.Fatalf("missing projection evidence %s:%s in %s", kind, ref, encoded)
	return RunProjectionEvidence{}
}

func assertProjectionEdge(t *testing.T, projection RunGraphProjection, source, target, kind string) {
	t.Helper()
	for _, edge := range projection.Edges {
		if edge.Source == source && edge.Target == target && edge.Kind == kind {
			return
		}
	}
	encoded, _ := json.MarshalIndent(projection.Edges, "", "  ")
	t.Fatalf("missing projection edge %s --%s--> %s in %s", source, kind, target, encoded)
}
