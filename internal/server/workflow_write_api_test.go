package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeWorkflowWriteStore struct {
	fakeReadStore
	workflow Workflow
	project  string
	name     string
	upsert   WorkflowRegister
	patchReq WorkflowPatchRequest
	err      error
}

func (s *fakeWorkflowWriteStore) UpsertWorkflow(_ context.Context, req WorkflowRegister) (Workflow, error) {
	s.upsert = req
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowWriteStore) DeleteWorkflow(_ context.Context, project string, name string) (Workflow, error) {
	s.project = project
	s.name = name
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func (s *fakeWorkflowWriteStore) PatchWorkflow(_ context.Context, project string, name string, req WorkflowPatchRequest) (Workflow, error) {
	s.project = project
	s.name = name
	s.patchReq = req
	if s.err != nil {
		return Workflow{}, s.err
	}
	return s.workflow, nil
}

func workflowRegisterBody(t *testing.T, req WorkflowRegister) *strings.Reader {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal workflow register: %v", err)
	}
	return strings.NewReader(string(data))
}

func verificationCaseJobsForTest() []NativeJobSpec {
	return boundedVerificationJobsForTest(DefaultVerificationBoundedCaseCount)
}

func singleVerificationJobForTest() NativeJobSpec {
	return NativeJobSpec{
		ID:             "llm-verify",
		Managed:        true,
		TimeoutSeconds: intPtr(2400),
		Steps: []NativeStepSpec{
			{Slug: "clone", Run: "echo clone"},
			{Slug: "prepare", Run: "echo prepare"},
			{Slug: "prepare-agent-workspace", Run: "echo prepare-agent"},
			{Slug: "run-verification", Run: "echo verify"},
			{Slug: "collect", Run: "echo collect"},
			{Slug: "finalize", Run: "echo finalize"},
			{Slug: "upload-screenshots", Run: "echo upload"},
			{Slug: "emit", Run: "echo emit"},
		},
	}
}

func boundedVerificationJobsForTest(count int) []NativeJobSpec {
	jobs := make([]NativeJobSpec, 0, count)
	for i := 1; i <= count; i++ {
		jobs = append(jobs, NativeJobSpec{
			ID:             verificationCaseJobID(i),
			TimeoutSeconds: intPtr(300),
		})
	}
	return jobs
}

func verificationJobForTest() NativeJobSpec {
	groupTitle := "Test cases generated at runtime"
	dynamicGroup := &StepDynamicGroup{MaxItems: MaxVerificationDynamicBlockItemCount, ItemLabel: "test case"}
	return NativeJobSpec{
		ID:             "verify",
		Managed:        true,
		TimeoutSeconds: intPtr(1800),
		Steps: []NativeStepSpec{
			{Slug: "author-test-plan", Run: "echo plan"},
			{Slug: "gather-evidence", Run: "echo gather", Group: "test-cases", GroupTitle: &groupTitle, DynamicGroup: dynamicGroup},
			{Slug: "judge-evidence", Run: "echo judge", Group: "test-cases", GroupTitle: &groupTitle, DynamicGroup: dynamicGroup},
			{Slug: "aggregate-verification", Run: "echo aggregate"},
		},
	}
}

func TestRegisterWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestRegisterWorkflowUpsertsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{
		fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}},
		workflow: Workflow{
			ID:        "agent-run",
			Project:   "ambience",
			Name:      "agent-run",
			CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
		},
	}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: PRTouchpointJobID, Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}},
		},
	}))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upsert.Project != "ambience" || store.upsert.Name != "agent-run" {
		t.Fatalf("upsert=%#v", store.upsert)
	}
	if len(store.upsert.Phases) != 6 {
		t.Fatalf("phases=%#v", store.upsert.Phases)
	}
	if store.upsert.Phases[0].Kind != "k8s_job" || store.upsert.Phases[0].WorkflowRef != "main" {
		t.Fatalf("phase defaults=%#v", store.upsert.Phases[0])
	}
}

func TestRegisterWorkflowRequiresProject(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, fakeAdminAuthenticator{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestRegisterWorkflowDefaultsBlankKindToK8sJob(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{
		ID:       "glimmung",
		Name:     "glimmung",
		Metadata: map[string]any{"app_type": "native_web_app"},
	}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: PRTouchpointJobID, Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}},
		},
	}))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.upsert.Phases[0].Kind != "k8s_job" || store.upsert.Phases[1].Kind != "k8s_job" || store.upsert.Phases[2].Kind != "k8s_job" {
		t.Fatalf("phase kinds=%#v", store.upsert.Phases)
	}
}

func TestRegisterWorkflowAcceptsParallelJobsInsideStrictPhase(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: "prepare"}, {ID: IssueContractJobID}}},
			{Name: "work", DependsOn: []string{"prepare"}, Jobs: []NativeJobSpec{{ID: "test-plan"}, {ID: "implement"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"work"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: PRTouchpointJobID, Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := len(store.upsert.Phases[1].Jobs); got != 2 {
		t.Fatalf("work jobs=%d, want 2", got)
	}
}

func TestValidateWorkflowRegisterAcceptsManagedRunSteps(t *testing.T) {
	req := WorkflowRegister{
		Name: "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{
				ID:      IssueContractJobID,
				Image:   "runner:latest",
				Managed: true,
				Steps: []NativeStepSpec{{
					Slug: "checkout",
					Run:  "echo ready",
				}},
			}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
	if got := req.Phases[0].Jobs[0].Steps[0].Type; got != "run" {
		t.Fatalf("managed run step type=%q, want run", got)
	}
}

func TestValidateWorkflowRegisterAcceptsManagedAgentSteps(t *testing.T) {
	req := WorkflowRegister{
		Name: "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Kind: "k8s_job", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{
				ID:      IssueContractJobID,
				Image:   "runner:latest",
				Managed: true,
				Steps: []NativeStepSpec{{
					Slug:  "implement",
					Type:  "agent",
					Agent: &AgentStepSpec{Slot: "implementation", Prompt: "ship it"},
				}},
			}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowRegisterAcceptsSingleVerificationJobConstraint(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeSingleJob
	req.Phases[1].Jobs = []NativeJobSpec{singleVerificationJobForTest()}

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowRegisterAcceptsBoundedCaseJobsConstraint(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeBoundedCaseJobs
	req.Constraints.Verification.MaxCases = DefaultVerificationBoundedCaseCount
	req.Phases[1].Jobs = boundedVerificationJobsForTest(DefaultVerificationBoundedCaseCount)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowRegisterRequiresVerificationCaseSlots(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeBoundedCaseJobs
	req.Phases[1].Jobs = []NativeJobSpec{{ID: "verify", TimeoutSeconds: intPtr(300)}}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly 10 bounded case jobs") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want bounded case slot rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresVerificationCaseJobIDs(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeBoundedCaseJobs
	req.Phases[1].Jobs[2].ID = "verify-extra"

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), `want "verify-case-03"`) {
		t.Fatalf("ValidateWorkflowRegister err=%v, want case id rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresVerificationCaseTimeout(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeBoundedCaseJobs
	req.Phases[1].Jobs[0].TimeoutSeconds = nil

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must set timeout_seconds") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing case timeout rejection", err)
	}
}

func TestValidateWorkflowRegisterCapsVerificationCaseTimeout(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeBoundedCaseJobs
	req.Phases[1].Jobs[0].TimeoutSeconds = intPtr(MaxVerificationCaseJobTimeoutSeconds + 1)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want case timeout ceiling rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresDynamicVerificationJobWhenConstrained(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []NativeJobSpec{verificationJobForTest(), {ID: "verify-extra"}}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one sequential verification job") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want single verification job rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresVerificationDynamicBlock(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []NativeJobSpec{verificationJobForTest()}
	req.Phases[1].Jobs[0].Steps[1].DynamicGroup = nil
	req.Phases[1].Jobs[0].Steps[2].DynamicGroup = nil

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must declare a dynamic test-case block") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want dynamic block rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresVerificationDynamicBlockGroup(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []NativeJobSpec{verificationJobForTest()}
	req.Phases[1].Jobs[0].Steps[1].Group = ""

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "declares dynamic_group without group") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want dynamic group rejection", err)
	}
}

func TestValidateWorkflowRegisterCapsVerificationDynamicBlockItems(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []NativeJobSpec{verificationJobForTest()}
	req.Phases[1].Jobs[0].Steps[1].DynamicGroup.MaxItems = MaxVerificationDynamicBlockItemCount + 1

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "dynamic_group.max_items") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want max items rejection", err)
	}
}

func TestValidateWorkflowRegisterRejectsSplitVerificationDynamicBlock(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []NativeJobSpec{verificationJobForTest()}
	job := &req.Phases[1].Jobs[0]
	job.Steps = []NativeStepSpec{
		job.Steps[0],
		job.Steps[1],
		{Slug: "between", Run: "echo between"},
		job.Steps[2],
		job.Steps[3],
	}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "one contiguous step block") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want split dynamic block rejection", err)
	}
}

func TestValidateWorkflowRegisterRejectsMultipleVerificationPhases(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	extra := PhaseSpec{
		Name:      "verify-again",
		Verify:    true,
		DependsOn: []string{"verify"},
		Jobs:      verificationCaseJobsForTest(),
	}
	req.Phases = append(req.Phases[:2], append([]PhaseSpec{extra}, req.Phases[2:]...)...)
	req.Phases[3].DependsOn = []string{"verify-again"}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one bounded verification phase") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want multiple verify phase rejection", err)
	}
}

func TestNormalizeWorkflowRegisterCanonicalizesEvidenceGate(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{
				Name:                     "gate",
				EvidenceVerificationGate: true,
				DependsOn:                []string{"verify"},
				Jobs: []NativeJobSpec{{
					ID:      "custom-gate",
					Image:   "python:3.12-slim",
					Command: []string{"python", "-c"},
					Args:    []string{"exit(1)"},
				}},
			},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
	gate := req.Phases[2]
	if len(gate.Jobs) != 1 {
		t.Fatalf("gate jobs=%#v", gate.Jobs)
	}
	job := gate.Jobs[0]
	if job.ID != "custom-gate" || !job.Managed || job.Image != "" || len(job.Command) != 0 || len(job.Args) != 0 {
		t.Fatalf("gate job=%#v", job)
	}
	if len(job.Steps) != 1 || job.Steps[0].Slug != EvidenceGateStepSlug || !strings.Contains(job.Steps[0].Run, "GLIMMUNG_INPUT_VERIFICATION") {
		t.Fatalf("gate steps=%#v", job.Steps)
	}
}

func TestCanonicalWorkflowCanonicalizesDeclaredPRTouchpointPrimitive(t *testing.T) {
	wf := Workflow{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "work", Jobs: []NativeJobSpec{{ID: "work"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"work"}, Jobs: []NativeJobSpec{
				{ID: "env-destroy"},
				{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint, Image: "ignored:latest", Command: []string{"ignored"}, TimeoutSeconds: intPtr(60)},
			}},
		},
	}

	got := CanonicalWorkflow(wf)

	if len(got.Phases) != 2 {
		t.Fatalf("phase count=%d, want 2", len(got.Phases))
	}
	cleanup := got.Phases[1]
	if len(cleanup.Jobs) != 2 {
		t.Fatalf("cleanup jobs=%#v", cleanup.Jobs)
	}
	if cleanup.Jobs[0].ID != "env-destroy" || cleanup.Jobs[1].ID != "publish-pr" {
		t.Fatalf("cleanup job ids=%q,%q", cleanup.Jobs[0].ID, cleanup.Jobs[1].ID)
	}
	job := cleanup.Jobs[1]
	if job.ID != "publish-pr" || job.Primitive != JobPrimitivePRTouchpoint || !job.Managed || job.Image != "" || len(job.Command) != 0 {
		t.Fatalf("pr touchpoint job=%#v", job)
	}
	if job.TimeoutSeconds == nil || *job.TimeoutSeconds != 60 {
		t.Fatalf("timeout=%v, want 60", job.TimeoutSeconds)
	}
	if len(job.Steps) != 1 || job.Steps[0].Slug != PRTouchpointStepSlug || !strings.Contains(job.Steps[0].Run, "GLIMMUNG_PR_TOUCHPOINT_URL") {
		t.Fatalf("pr touchpoint job=%#v", job)
	}
}

func TestValidateWorkflowRegisterRequiresPRTouchpoint(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "pr_touchpoint") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing pr_touchpoint", err)
	}
}

func TestValidateWorkflowRegisterRequiresPRTouchpointInReviewTouchpointPhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "publish", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeWork, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"publish"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "purpose=\"review_touchpoint\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want review-touchpoint error", err)
	}
}

func TestValidateWorkflowRegisterRejectsMultiplePrTouchpointJobs(t *testing.T) {
	// Two pr_touchpoint primitives is invalid even without a gate.
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{
				{ID: "publish-pr-a", Primitive: JobPrimitivePRTouchpoint},
				{ID: "publish-pr-b", Primitive: JobPrimitivePRTouchpoint},
			}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one is required") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want multiple-pr_touchpoint error", err)
	}
}

func TestValidateWorkflowRegisterAcceptsTouchpointGatePhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister err=%v, want nil for valid touchpoint_gate shape", err)
	}
}

func TestValidateWorkflowRegisterRejectsTouchpointGateWithoutMergeJob(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"verify"}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must declare exactly one job with primitive \"pr_merge\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing-pr_merge error", err)
	}
}

func TestValidateWorkflowRegisterRejectsPRMergeOutsideGate(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Jobs: []NativeJobSpec{{ID: "prepare"}, {ID: "rogue-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must live inside a purpose=\"review_gate\" phase") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want pr_merge-outside-gate error", err)
	}
}

func TestValidateWorkflowRegisterRejectsTouchpointGateMarkedVerify(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, Verify: true, DependsOn: []string{"prepare"}},
			{Name: "verify", Verify: true, DependsOn: []string{"touchpoint_gate"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "has verify=true and must set purpose") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want verify-conflict error", err)
	}
}

func TestValidateWorkflowRegisterRejectsUnknownPhaseKind(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Kind: "mystery_kind", Jobs: []NativeJobSpec{{ID: "prepare"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	// deliberately do NOT call normalizeWorkflowRegister so the unknown kind survives.

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want unknown-kind error", err)
	}
}

func TestValidateWorkflowRegisterRejectsTouchpointGateExecutorKind(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "touchpoint_gate", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), `kind "touchpoint_gate" is not one of [k8s_job]`) {
		t.Fatalf("ValidateWorkflowRegister err=%v, want touchpoint_gate executor rejection", err)
	}
}

func TestValidateWorkflowRegisterRejectsUnknownJobPrimitive(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Jobs: []NativeJobSpec{{ID: "prepare", Primitive: "mystery"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "unknown primitive") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want unknown primitive", err)
	}
}

func TestValidateWorkflowRegisterRejectsInvalidManagedSteps(t *testing.T) {
	base := func(job NativeJobSpec) WorkflowRegister {
		return WorkflowRegister{
			Name: "agent-run",
			Phases: []PhaseSpec{
				{Name: "prepare", Jobs: []NativeJobSpec{job}},
				{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
				{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}},
			},
		}
	}
	tests := []struct {
		name string
		job  NativeJobSpec
		want string
	}{
		{
			name: "command",
			job:  NativeJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Command: []string{"bash"}, Steps: []NativeStepSpec{{Slug: "s", Run: "echo ok"}}},
			want: "cannot declare command or args",
		},
		{
			name: "missing run",
			job:  NativeJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Steps: []NativeStepSpec{{Slug: "s"}}},
			want: "is missing run",
		},
		{
			name: "duplicate step",
			job:  NativeJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Steps: []NativeStepSpec{{Slug: "s", Run: "echo one"}, {Slug: "s", Run: "echo two"}}},
			want: "duplicates step",
		},
		{
			name: "unsupported type",
			job:  NativeJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Steps: []NativeStepSpec{{Slug: "s", Type: "other", Run: "codex"}}},
			want: "unsupported type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base(tt.job)
			normalizeWorkflowRegister(&req)
			err := ValidateWorkflowRegister(req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateWorkflowRegister error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestRegisterWorkflowRejectsNonNativeKind(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{
		ID:       "glimmung",
		Name:     "glimmung",
		Metadata: map[string]any{"native_webapp": true},
	}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"glimmung","name":"agent-run","phases":[{"name":"prepare","kind":"container"},{"name":"verify","kind":"k8s_job","verify":true,"depends_on":["prepare"]},{"name":"cleanup","kind":"k8s_job","run_on":"always","purpose":"teardown","depends_on":["verify"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegisterWorkflowRequiresMandatoryPhases(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"verify","verify":true}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestValidateWorkflowRegisterRequiresPrepareEntryPhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "env-prep", Jobs: []NativeJobSpec{{ID: "env-prep"}}},
			{Name: "verify", Verify: true, DependsOn: []string{"env-prep"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), `entry phase must be named "prepare"`) {
		t.Fatalf("ValidateWorkflowRegister err=%v, want prepare entry rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresPrepareIssueContract(t *testing.T) {
	t.Run("missing output", func(t *testing.T) {
		req := workflowWithJobTimeout(nil)
		req.Phases[0].Outputs = nil
		err := ValidateWorkflowRegister(req)
		if err == nil || !strings.Contains(err.Error(), IssueContractOutputKey) {
			t.Fatalf("ValidateWorkflowRegister err=%v, want issue_contract output rejection", err)
		}
	})
	t.Run("missing job", func(t *testing.T) {
		req := workflowWithJobTimeout(nil)
		req.Phases[0].Jobs = []NativeJobSpec{{ID: "env-prep"}}
		err := ValidateWorkflowRegister(req)
		if err == nil || !strings.Contains(err.Error(), IssueContractJobID) {
			t.Fatalf("ValidateWorkflowRegister err=%v, want issue-contract job rejection", err)
		}
	})
}

func TestRegisterWorkflowRejectsMultipleEntryPhases(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}},
		},
	}))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "must declare exactly one depends_on") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRegisterWorkflowRejectsEvidenceGateWithoutVerifyProducer(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prepare","outputs":["issue_contract"],"jobs":[{"id":"issue-contract"}]},{"name":"gate","evidence_verification_gate":true,"depends_on":["prepare"]},{"name":"cleanup","run_on":"always","purpose":"teardown","depends_on":["gate"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verify") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestRegisterWorkflowRejectsBadPhaseInputRef(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"validation_url", IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Inputs: map[string]string{"missing": "${{ phases.prepare.outputs.nope }}"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: PRTouchpointJobID, Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}},
		},
	}))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "doesn't declare that output") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestPatchWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{"budget_total":50}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestPatchWorkflowPatchesAndReturnsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:        "agent-run",
		Project:   "ambience",
		Name:      "agent-run",
		CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{"budget_total":50}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "ambience" || store.name != "agent-run" {
		t.Fatalf("project=%q name=%q", store.project, store.name)
	}
	if store.patchReq.BudgetTotal == nil || *store.patchReq.BudgetTotal != 50 {
		t.Fatalf("budget_total=%v", store.patchReq.BudgetTotal)
	}
}

func TestPatchWorkflowMapsMissingTo404(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ErrNotFound},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/missing", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestPatchWorkflowStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: errors.New("boom")},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestPatchWorkflowDecodesRecycleMaxAttempts(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:      "agent-run",
		Project: "ambience",
		Name:    "agent-run",
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	body := `{"recycle_max_attempts":[{"target":"evidence-gate","max_attempts":5},{"target":"pr","max_attempts":2}]}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.patchReq.RecycleMaxAttempts) != 2 {
		t.Fatalf("recycle_max_attempts=%v", store.patchReq.RecycleMaxAttempts)
	}
	if store.patchReq.RecycleMaxAttempts[0] != (RecycleMaxAttemptsPatch{Target: "evidence-gate", MaxAttempts: 5}) {
		t.Fatalf("patch[0]=%v", store.patchReq.RecycleMaxAttempts[0])
	}
	if store.patchReq.RecycleMaxAttempts[1] != (RecycleMaxAttemptsPatch{Target: RecyclePatchTargetPR, MaxAttempts: 2}) {
		t.Fatalf("patch[1]=%v", store.patchReq.RecycleMaxAttempts[1])
	}
}

func TestPatchWorkflowMapsValidationErrorTo400(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ValidationError{Message: "bad recycle target"}},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	body := `{"recycle_max_attempts":[{"target":"nope","max_attempts":2}]}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/workflows/ambience/agent-run", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 body=%s", rec.Code, rec.Body.String())
	}
}

func recycleTestRegister() WorkflowRegister {
	return WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare"},
			{Name: "evidence-gate", RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}, LandsAt: "prepare"}},
		},
		PR: PrPrimitive{RecyclePolicy: &RecyclePolicy{MaxAttempts: 3, On: []string{"pr_review_changes_requested"}, LandsAt: "prepare"}},
	}
}

func TestApplyRecycleMaxAttemptsPatchesScalesPhaseAndPR(t *testing.T) {
	reg := recycleTestRegister()
	err := ApplyRecycleMaxAttemptsPatches(&reg, []RecycleMaxAttemptsPatch{
		{Target: "evidence-gate", MaxAttempts: 5},
		{Target: RecyclePatchTargetPR, MaxAttempts: 1},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := reg.Phases[1].RecyclePolicy.MaxAttempts; got != 5 {
		t.Fatalf("phase max_attempts=%d, want 5", got)
	}
	if got := reg.PR.RecyclePolicy.MaxAttempts; got != 1 {
		t.Fatalf("pr max_attempts=%d, want 1", got)
	}
	// Structural fields must be untouched.
	if reg.Phases[1].RecyclePolicy.LandsAt != "prepare" || len(reg.Phases[1].RecyclePolicy.On) != 1 {
		t.Fatalf("structural fields mutated: %+v", reg.Phases[1].RecyclePolicy)
	}
}

func TestApplyRecycleMaxAttemptsPatchesRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		patch RecycleMaxAttemptsPatch
	}{
		{"unknown phase", RecycleMaxAttemptsPatch{Target: "ghost", MaxAttempts: 2}},
		{"phase without policy", RecycleMaxAttemptsPatch{Target: "prepare", MaxAttempts: 2}},
		{"zero attempts", RecycleMaxAttemptsPatch{Target: "evidence-gate", MaxAttempts: 0}},
		{"over ceiling", RecycleMaxAttemptsPatch{Target: "evidence-gate", MaxAttempts: MaxRecycleMaxAttempts + 1}},
		{"empty target", RecycleMaxAttemptsPatch{Target: "", MaxAttempts: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := recycleTestRegister()
			err := ApplyRecycleMaxAttemptsPatches(&reg, []RecycleMaxAttemptsPatch{tc.patch})
			if _, ok := err.(ValidationError); !ok {
				t.Fatalf("err=%v, want ValidationError", err)
			}
			// A rejected request must not mutate the register.
			if reg.Phases[1].RecyclePolicy.MaxAttempts != 3 {
				t.Fatalf("phase mutated despite rejection: %d", reg.Phases[1].RecyclePolicy.MaxAttempts)
			}
		})
	}
}

func TestApplyRecycleMaxAttemptsPatchesRejectsDuplicateTarget(t *testing.T) {
	reg := recycleTestRegister()
	err := ApplyRecycleMaxAttemptsPatches(&reg, []RecycleMaxAttemptsPatch{
		{Target: "evidence-gate", MaxAttempts: 4},
		{Target: "evidence-gate", MaxAttempts: 5},
	})
	if _, ok := err.(ValidationError); !ok {
		t.Fatalf("err=%v, want ValidationError", err)
	}
}

func TestApplyRecycleMaxAttemptsPatchesNoopOnEmpty(t *testing.T) {
	reg := recycleTestRegister()
	if err := ApplyRecycleMaxAttemptsPatches(&reg, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.Phases[1].RecyclePolicy.MaxAttempts != 3 {
		t.Fatalf("phase mutated: %d", reg.Phases[1].RecyclePolicy.MaxAttempts)
	}
}

func TestDeleteWorkflowRequiresAdmin(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeWorkflowWriteStore{}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestDeleteWorkflowDeletesAndReturnsWorkflow(t *testing.T) {
	store := &fakeWorkflowWriteStore{workflow: Workflow{
		ID:        "agent-run",
		Project:   "ambience",
		Name:      "agent-run",
		CreatedAt: time.Date(2026, 5, 11, 3, 0, 0, 0, time.UTC),
	}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.project != "ambience" || store.name != "agent-run" {
		t.Fatalf("project=%q name=%q", store.project, store.name)
	}
	if !strings.Contains(rec.Body.String(), `"name":"agent-run"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestDeleteWorkflowMapsMissingTo404(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: ErrNotFound},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/missing", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestDeleteWorkflowStoreErrorsReturn500(t *testing.T) {
	handler := NewWithDependencies(
		Settings{},
		&fakeWorkflowWriteStore{err: errors.New("boom")},
		fakeAdminAuthenticator{},
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflows/ambience/agent-run", nil)
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// workflowWithJobTimeout produces a valid workflow shape with a single
// configurable timeout on the entry phase's job — used by the timeout
// guardrail tests below.
func workflowWithJobTimeout(timeout *int) WorkflowRegister {
	return WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID, TimeoutSeconds: timeout}}},
			{Name: "verify", Verify: true, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, DependsOn: []string{"verify"}, Jobs: []NativeJobSpec{{ID: "cleanup-early"}}},
			{Name: "touchpoint", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, DependsOn: []string{"cleanup_early"}, Jobs: []NativeJobSpec{{ID: "pr-touchpoint", Primitive: JobPrimitivePRTouchpoint}}},
			{Name: "touchpoint_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"touchpoint_gate"}, Jobs: []NativeJobSpec{{ID: "cleanup-final"}}},
		},
	}
}

func TestValidateWorkflowRejectsTimeoutBelowFloor(t *testing.T) {
	err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MinNativePhaseJobTimeoutSeconds - 1)))
	if err == nil || !strings.Contains(err.Error(), "below minimum") {
		t.Fatalf("err=%v, want below-minimum rejection", err)
	}
}

func TestValidateWorkflowRejectsTimeoutAboveCeiling(t *testing.T) {
	err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MaxNativePhaseJobTimeoutSeconds + 1)))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("err=%v, want above-maximum rejection", err)
	}
}

func TestValidateWorkflowAcceptsTimeoutAtFloor(t *testing.T) {
	if err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MinNativePhaseJobTimeoutSeconds))); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowAcceptsNilTimeout(t *testing.T) {
	if err := ValidateWorkflowRegister(workflowWithJobTimeout(nil)); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}
