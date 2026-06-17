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

	"github.com/romaine-life/glimmung/internal/runnermcp"
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

func (s *fakeWorkflowWriteStore) DeleteWorkflow(_ context.Context, project string, name string, _ string) (Workflow, error) {
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

func verificationCaseJobsForTest() []RunnerJobSpec {
	return boundedVerificationJobsForTest(DefaultVerificationBoundedCaseCount)
}

func singleVerificationJobForTest() RunnerJobSpec {
	return RunnerJobSpec{
		ID:             "llm-verify",
		Managed:        true,
		TimeoutSeconds: intPtr(2400),
		Steps: []RunnerStepSpec{
			{Slug: "clone", Run: "echo clone"},
			{Slug: "prepare", Run: "echo prepare"},
			{Slug: "prepare-agent-workspace", Run: "echo prepare-agent"},
			{Slug: "run-verification", Run: "echo verify"},
			{Slug: "collect", Run: "echo collect"},
			{Slug: VerificationStepSlug, Primitive: StepPrimitiveVerificationFinalize},
		},
	}
}

func boundedVerificationJobsForTest(count int) []RunnerJobSpec {
	jobs := make([]RunnerJobSpec, 0, count)
	for i := 1; i <= count; i++ {
		jobs = append(jobs, RunnerJobSpec{
			ID:             verificationCaseJobID(i),
			TimeoutSeconds: intPtr(300),
		})
	}
	return jobs
}

func verificationJobForTest() RunnerJobSpec {
	groupTitle := "Test cases generated at runtime"
	dynamicGroup := &StepDynamicGroup{MaxItems: MaxVerificationDynamicBlockItemCount, ItemLabel: "test case"}
	return RunnerJobSpec{
		ID:             "verify",
		Managed:        true,
		TimeoutSeconds: intPtr(1800),
		Steps: []RunnerStepSpec{
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
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: PRReviewJobID, Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}},
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
	if store.upsert.Phases[0].Kind != "k8s_job" || store.upsert.Phases[0].WorkflowRef != CanonicalGitRefDefault {
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
		Metadata: map[string]any{"app_type": "webapp"},
	}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "glimmung",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: PRReviewJobID, Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}},
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
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "prepare"}, {ID: "issue-contract"}}},
			{Name: "work", DependsOn: []string{"prepare"}, Jobs: []RunnerJobSpec{{ID: "test-plan"}, {ID: "implement"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"work"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup-early"}}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: PRReviewJobID, Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-final"}}},
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
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{
				ID:      "issue-contract",
				Image:   "runner:latest",
				Managed: true,
				Steps: []RunnerStepSpec{{
					Slug: "checkout",
					Run:  "echo ready",
				}},
			}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup-early"}}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-final"}}},
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

func TestValidateRunnerJobSpecTools(t *testing.T) {
	job := func(tools []string, managed bool) RunnerJobSpec {
		return RunnerJobSpec{ID: "implement", Image: "runner:latest", Managed: managed, Tools: tools}
	}

	if err := validateRunnerJobSpec("agent-run", "work", 0, job([]string{runnermcp.ToolUploadEvidence}, true)); err != nil {
		t.Fatalf("known tool on managed job must be accepted: %v", err)
	}
	if err := validateRunnerJobSpec("agent-run", "work", 0, job(nil, true)); err != nil {
		t.Fatalf("a job with no tools must be accepted: %v", err)
	}

	err := validateRunnerJobSpec("agent-run", "work", 0, job([]string{"capture_unicorns"}, true))
	if err == nil || !strings.Contains(err.Error(), "unknown runner tool") || !strings.Contains(err.Error(), "capture_unicorns") {
		t.Fatalf("unknown tool must be rejected and named, got %v", err)
	}
	// The rejection must advertise the known tools so an author can self-correct.
	if err == nil || !strings.Contains(err.Error(), runnermcp.ToolUploadEvidence) {
		t.Fatalf("unknown-tool rejection must list known tools, got %v", err)
	}

	err = validateRunnerJobSpec("agent-run", "work", 0, job([]string{runnermcp.ToolUploadEvidence}, false))
	if err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("tools on an unmanaged job must be rejected, got %v", err)
	}

	err = validateRunnerJobSpec("agent-run", "work", 0, job([]string{""}, true))
	if err == nil || !strings.Contains(err.Error(), "empty tool name") {
		t.Fatalf("empty tool name must be rejected, got %v", err)
	}

	err = validateRunnerJobSpec("agent-run", "work", 0, job([]string{runnermcp.ToolUploadEvidence, runnermcp.ToolUploadEvidence}, true))
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("a duplicated tool must be rejected, got %v", err)
	}
}

func TestValidateWorkflowRegisterAcceptsManagedAgentSteps(t *testing.T) {
	req := WorkflowRegister{
		Name: "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Kind: "k8s_job", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{
				ID:      "issue-contract",
				Image:   "runner:latest",
				Managed: true,
				Steps: []RunnerStepSpec{{
					Slug:  "implement",
					Type:  "agent",
					Agent: &AgentStepSpec{Slot: "implementation", Prompt: "ship it"},
				}},
			}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup-early"}}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-final"}}},
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
	req.Phases[1].Jobs = []RunnerJobSpec{singleVerificationJobForTest()}

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowRegisterRequiresSingleVerificationFinalizer(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeSingleJob
	req.Phases[1].Jobs = []RunnerJobSpec{{
		ID:      "llm-verify",
		Managed: true,
		Steps: []RunnerStepSpec{
			{Slug: "run-verification", Run: "echo verify"},
			{Slug: "emit-verification", Run: "echo emit"},
		},
	}}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), StepPrimitiveVerificationFinalize) {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing finalizer rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresSingleVerificationFinalizerLast(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeSingleJob
	job := singleVerificationJobForTest()
	job.Steps = append(job.Steps, RunnerStepSpec{Slug: "after-finalizer", Run: "echo late"})
	req.Phases[1].Jobs = []RunnerJobSpec{job}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must be the final step") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want finalizer-last rejection", err)
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
	req.Phases[1].Jobs = []RunnerJobSpec{{ID: "verify", TimeoutSeconds: intPtr(300)}}

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
	req.Phases[1].Jobs = []RunnerJobSpec{verificationJobForTest(), {ID: "verify-extra"}}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one sequential verification job") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want single verification job rejection", err)
	}
}

func TestValidateWorkflowRegisterRequiresVerificationDynamicBlock(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []RunnerJobSpec{verificationJobForTest()}
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
	req.Phases[1].Jobs = []RunnerJobSpec{verificationJobForTest()}
	req.Phases[1].Jobs[0].Steps[1].Group = ""

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "declares dynamic_group without group") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want dynamic group rejection", err)
	}
}

func TestValidateWorkflowRegisterCapsVerificationDynamicBlockItems(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []RunnerJobSpec{verificationJobForTest()}
	req.Phases[1].Jobs[0].Steps[1].DynamicGroup.MaxItems = MaxVerificationDynamicBlockItemCount + 1

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "dynamic_group.max_items") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want max items rejection", err)
	}
}

func TestValidateWorkflowRegisterRejectsSplitVerificationDynamicBlock(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Constraints.Verification.Shape = VerificationShapeDynamicStepGroup
	req.Phases[1].Jobs = []RunnerJobSpec{verificationJobForTest()}
	job := &req.Phases[1].Jobs[0]
	job.Steps = []RunnerStepSpec{
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
		Name:          "verify-again",
		Verify:        true,
		RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"},
		DependsOn:     []string{"verify"},
		Jobs:          verificationCaseJobsForTest(),
	}
	req.Phases = append(req.Phases[:2], append([]PhaseSpec{extra}, req.Phases[2:]...)...)
	req.Phases[3].DependsOn = []string{"verify-again"}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one bounded verification phase") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want multiple verify phase rejection", err)
	}
}

func TestCanonicalWorkflowCanonicalizesLegacyEvidenceGate(t *testing.T) {
	wf := Workflow{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{
				Name:                     "gate",
				EvidenceVerificationGate: true,
				DependsOn:                []string{"verify"},
				Jobs: []RunnerJobSpec{{
					ID:      "custom-gate",
					Image:   "python:3.12-slim",
					Command: []string{"python", "-c"},
					Args:    []string{"exit(1)"},
				}},
			},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-early"}}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-final"}}},
		},
	}
	wf = CanonicalWorkflow(wf)
	gate := wf.Phases[2]
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

func TestCanonicalWorkflowCanonicalizesDeclaredPRReviewPrimitive(t *testing.T) {
	wf := Workflow{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "work", Jobs: []RunnerJobSpec{{ID: "work"}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"work"}, Jobs: []RunnerJobSpec{
				{ID: "env-destroy"},
				{ID: "publish-pr", Primitive: JobPrimitivePRReview, Image: "ignored:latest", Command: []string{"ignored"}, TimeoutSeconds: intPtr(60)},
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
	if job.ID != "publish-pr" || job.Primitive != JobPrimitivePRReview || !job.Managed || job.Image != "" || len(job.Command) != 0 {
		t.Fatalf("pr review job=%#v", job)
	}
	if job.TimeoutSeconds == nil || *job.TimeoutSeconds != 60 {
		t.Fatalf("timeout=%v, want 60", job.TimeoutSeconds)
	}
	if len(job.Steps) != 1 || job.Steps[0].Slug != PRReviewStepSlug || !strings.Contains(job.Steps[0].Run, "GLIMMUNG_PR_REVIEW_URL") {
		t.Fatalf("pr review job=%#v", job)
	}
}

func TestCanonicalWorkflowCanonicalizesVerificationFinalizerStep(t *testing.T) {
	wf := Workflow{
		Project: "spirelens",
		Name:    "default",
		Phases: []PhaseSpec{{
			Name:   "llm-verify",
			Verify: true,
			Jobs: []RunnerJobSpec{{
				ID:      "llm-verify",
				Managed: true,
				Steps: []RunnerStepSpec{
					{Slug: "run-verification", Run: "echo verify"},
					{Slug: "custom-finalize", Primitive: StepPrimitiveVerificationFinalize, Run: "ignored"},
				},
			}},
		}},
	}

	got := CanonicalWorkflow(wf)
	step := got.Phases[0].Jobs[0].Steps[1]
	if step.Slug != "custom-finalize" || step.Primitive != StepPrimitiveVerificationFinalize || step.Type != "run" {
		t.Fatalf("finalizer step=%#v", step)
	}
	if !strings.Contains(step.Run, "GLIMMUNG_COMPLETION_FILE") || !strings.Contains(step.Run, "ARTIFACTS_STORAGE_ACCOUNT") {
		t.Fatalf("finalizer run script missing contract envs: %s", step.Run)
	}
}

func TestValidateWorkflowRegisterRequiresPRReview(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "pr_review") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want missing pr_review", err)
	}
}

func TestValidateWorkflowRegisterRequiresPRReviewInReviewPhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "publish", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeWork, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"publish"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "purpose=\"review\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want review-review error", err)
	}
}

func TestValidateWorkflowRegisterRequiresReviewPhaseName(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "human-review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"human-review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must be named \"review\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want review phase name error", err)
	}
}

func TestValidateWorkflowRegisterRejectsMultiplePrReviewJobs(t *testing.T) {
	// Two pr_review primitives is invalid even without a gate.
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{
				{ID: "publish-pr-a", Primitive: JobPrimitivePRReview},
				{ID: "publish-pr-b", Primitive: JobPrimitivePRReview},
			}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "exactly one is required") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want multiple-pr_review error", err)
	}
}

func TestValidateWorkflowRegisterAcceptsReviewGatePhase(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		PR:      PrPrimitive{},
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister err=%v, want nil for valid review_gate shape", err)
	}
}

func TestValidateWorkflowRegisterRejectsReviewGateWithoutMergeJob(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"verify"}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
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
			{Name: "prepare", Jobs: []RunnerJobSpec{{ID: "prepare"}, {ID: "rogue-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must live inside a purpose=\"review_gate\" phase") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want pr_merge-outside-gate error", err)
	}
}

func TestValidateWorkflowRegisterRequiresReviewGateName(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRReview}}},
			{Name: "human-review-gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"human-review-gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "must be named \"review_gate\"") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want review_gate phase name error", err)
	}
}

func TestValidateWorkflowRegisterRejectsReviewGateMarkedVerify(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"review_gate"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
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
			{Name: "prepare", Kind: "mystery_kind", Jobs: []RunnerJobSpec{{ID: "prepare"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	// deliberately do NOT call normalizeWorkflowRegister so the unknown kind survives.

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want unknown-kind error", err)
	}
}

func TestValidateWorkflowRegisterRejectsReviewGateExecutorKind(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "publish-pr", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "review_gate", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), `kind "review_gate" is not one of [k8s_job]`) {
		t.Fatalf("ValidateWorkflowRegister err=%v, want review_gate executor rejection", err)
	}
}

func TestValidateWorkflowRegisterRejectsUnknownJobPrimitive(t *testing.T) {
	req := WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Jobs: []RunnerJobSpec{{ID: "prepare", Primitive: "mystery"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "unknown primitive") {
		t.Fatalf("ValidateWorkflowRegister err=%v, want unknown primitive", err)
	}
}

func TestValidateWorkflowRegisterRejectsInvalidManagedSteps(t *testing.T) {
	base := func(job RunnerJobSpec) WorkflowRegister {
		return WorkflowRegister{
			Name: "agent-run",
			Phases: []PhaseSpec{
				{Name: "prepare", Jobs: []RunnerJobSpec{job}},
				{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
				{Name: "cleanup", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"verify"}},
			},
		}
	}
	tests := []struct {
		name string
		job  RunnerJobSpec
		want string
	}{
		{
			name: "command",
			job:  RunnerJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Command: []string{"bash"}, Steps: []RunnerStepSpec{{Slug: "s", Run: "echo ok"}}},
			want: "cannot declare command or args",
		},
		{
			name: "missing run",
			job:  RunnerJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Steps: []RunnerStepSpec{{Slug: "s"}}},
			want: "is missing run",
		},
		{
			name: "duplicate step",
			job:  RunnerJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Steps: []RunnerStepSpec{{Slug: "s", Run: "echo one"}, {Slug: "s", Run: "echo two"}}},
			want: "duplicates step",
		},
		{
			name: "unsupported type",
			job:  RunnerJobSpec{ID: "prepare", Image: "runner:latest", Managed: true, Steps: []RunnerStepSpec{{Slug: "s", Type: "other", Run: "codex"}}},
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

func TestRegisterWorkflowRejectsNonK8sJobForWebapp(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{
		ID:       "glimmung",
		Name:     "glimmung",
		Metadata: map[string]any{"app_kind": "webapp"},
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
			{Name: "env-prep", Jobs: []RunnerJobSpec{{ID: "env-prep"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"env-prep"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup-early"}}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-final"}}},
		},
	}
	normalizeWorkflowRegister(&req)

	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), `entry phase must be named "prepare"`) {
		t.Fatalf("ValidateWorkflowRegister err=%v, want prepare entry rejection", err)
	}
}

// The platform must not mandate any project's stage names beyond the
// generic prepare/verify/teardown skeleton. The retired issue-contract
// entry-phase mandate ("entry phase must declare job issue-contract /
// output issue_contract") stays deleted: a prepare phase with only
// project-owned jobs registers cleanly.
func TestValidateWorkflowRegisterDoesNotMandateIssueContract(t *testing.T) {
	req := workflowWithJobTimeout(nil)
	req.Phases[0].Outputs = []string{"validation_url"}
	req.Phases[0].Jobs = []RunnerJobSpec{{ID: "env-prep"}}
	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("a prepare phase without an issue-contract job must register: %v", err)
	}
}

func TestRegisterWorkflowRejectsMultipleEntryPhases(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", workflowRegisterBody(t, WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, Jobs: verificationCaseJobsForTest()},
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

func TestRegisterWorkflowRejectsRetiredEvidenceGate(t *testing.T) {
	store := &fakeWorkflowWriteStore{fakeReadStore: fakeReadStore{projects: []Project{{ID: "ambience", Name: "ambience"}}}}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(`{"project":"ambience","name":"agent-run","phases":[{"name":"prepare","outputs":["issue_contract"],"jobs":[{"id":"issue-contract"}]},{"name":"gate","evidence_verification_gate":true,"depends_on":["prepare"]},{"name":"cleanup","run_on":"always","purpose":"teardown","depends_on":["gate"]}]}`))
	req.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "retired evidence_verification_gate") {
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
			{Name: "prepare", Outputs: []string{"validation_url", "issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract"}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Inputs: map[string]string{"missing": "${{ phases.prepare.outputs.nope }}"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: PRReviewJobID, Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}},
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

func TestDeleteWorkflowTombstonesAndReturnsWorkflow(t *testing.T) {
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
			{Name: "prepare", Outputs: []string{"issue_contract"}, Jobs: []RunnerJobSpec{{ID: "issue-contract", TimeoutSeconds: timeout}}},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}, Jobs: []RunnerJobSpec{{ID: "cleanup-early"}}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: "pr-review", Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: "pr-merge", Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}, Jobs: []RunnerJobSpec{{ID: "cleanup-final"}}},
		},
	}
}

func TestValidateWorkflowRejectsTimeoutBelowFloor(t *testing.T) {
	err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MinRunnerPhaseJobTimeoutSeconds - 1)))
	if err == nil || !strings.Contains(err.Error(), "below minimum") {
		t.Fatalf("err=%v, want below-minimum rejection", err)
	}
}

func TestValidateWorkflowRejectsTimeoutAboveCeiling(t *testing.T) {
	err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MaxRunnerPhaseJobTimeoutSeconds + 1)))
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("err=%v, want above-maximum rejection", err)
	}
}

func TestValidateWorkflowAcceptsTimeoutAtFloor(t *testing.T) {
	if err := ValidateWorkflowRegister(workflowWithJobTimeout(intPtr(MinRunnerPhaseJobTimeoutSeconds))); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowAcceptsNilTimeout(t *testing.T) {
	if err := ValidateWorkflowRegister(workflowWithJobTimeout(nil)); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

// dispatchInputsWorkflowFixture builds a valid registration the
// dispatch_inputs tests can mutate. The prepare phase carries a checkout
// whose ref is the template `${{ inputs.git_ref }}`; declaring `git_ref` on
// the workflow is what each test toggles.
func dispatchInputsWorkflowFixture() WorkflowRegister {
	return WorkflowRegister{
		Project: "ambience",
		Name:    "agent-run",
		Phases: []PhaseSpec{
			{
				Name:    "prepare",
				Outputs: []string{"issue_contract"},
				Jobs: []RunnerJobSpec{
					{
						ID:       "issue-contract",
						Checkout: &RunnerCheckoutSpec{Ref: "${{ inputs.git_ref }}", Path: "/workspace/ambience"},
					},
				},
			},
			{Name: "verify", Verify: true, RecyclePolicy: &RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail"}, LandsAt: "prepare"}, DependsOn: []string{"prepare"}, Jobs: verificationCaseJobsForTest()},
			{Name: "cleanup_early", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, When: "${{ run.preserve_test_env }} == 'false'", DependsOn: []string{"verify"}},
			{Name: "review", RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReview, DependsOn: []string{"cleanup_early"}, Jobs: []RunnerJobSpec{{ID: PRReviewJobID, Primitive: JobPrimitivePRReview}}},
			{Name: "review_gate", Kind: "k8s_job", Purpose: PhasePurposeReviewGate, DependsOn: []string{"review"}, Jobs: []RunnerJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge}}},
			{Name: "cleanup_final", RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, DependsOn: []string{"review_gate"}},
		},
	}
}

func TestValidateWorkflowAcceptsDeclaredDispatchInput(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
	}
	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}

func TestValidateWorkflowRequiresGitRefForProjectCheckouts(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.Phases[0].Jobs[0].Checkout.Ref = "main"
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
	}
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "checkout.ref must be") || !strings.Contains(err.Error(), CanonicalGitRefTemplate) {
		t.Fatalf("err=%v, want canonical checkout ref rejection", err)
	}
}

func TestValidateWorkflowRequiresGitRefDeclarationForProjectCheckouts(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.Phases[0].Jobs[0].Checkout.Ref = "main"
	req.DispatchInputs = nil
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "dispatch_inputs") || !strings.Contains(err.Error(), CanonicalGitRefInput) {
		t.Fatalf("err=%v, want mandatory git_ref declaration rejection", err)
	}
}

func TestValidateWorkflowRequiresGitRefDefaultForProjectCheckouts(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true},
	}
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "non-empty default") || !strings.Contains(err.Error(), CanonicalGitRefInput) {
		t.Fatalf("err=%v, want git_ref default rejection", err)
	}
}

func TestValidateWorkflowRejectsUndeclaredDispatchInputRef(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	// DispatchInputs intentionally empty; the prepare checkout still
	// references ${{ inputs.git_ref }}.
	err := ValidateWorkflowRegister(req)
	if err == nil {
		t.Fatalf("ValidateWorkflowRegister: want rejection, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "git_ref") || !strings.Contains(msg, "dispatch_inputs does not declare") {
		t.Fatalf("err=%q, want git_ref + dispatch_inputs does not declare", msg)
	}
}

func TestValidateWorkflowRejectsUndeclaredDispatchInputRefInExtraCheckouts(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
	}
	req.Phases[0].Jobs[0].ExtraCheckouts = []RunnerCheckoutSpec{
		{Repo: "owner/extras", Ref: "${{ inputs.extras_ref }}", Path: "/workspace/extras"},
	}
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "extras_ref") {
		t.Fatalf("err=%v, want extras_ref rejection", err)
	}
}

func TestValidateWorkflowRejectsUndeclaredDispatchInputRefInWorkflowRef(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.Phases[0].WorkflowRef = "${{ inputs.git_ref }}"
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "git_ref") {
		t.Fatalf("err=%v, want git_ref rejection", err)
	}
}

func TestValidateWorkflowRejectsDuplicateDispatchInputName(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
	}
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("err=%v, want duplicate rejection", err)
	}
}

func TestValidateWorkflowRejectsInvalidDispatchInputName(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: "bad name", Required: true},
	}
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err=%v, want invalid name rejection", err)
	}
}

func TestValidateWorkflowRejectsNonRequiredInputWithoutDefault(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
		{Name: "extras_ref", Required: false},
	}
	err := ValidateWorkflowRegister(req)
	if err == nil || !strings.Contains(err.Error(), "not required but has no default") {
		t.Fatalf("err=%v, want missing-default rejection", err)
	}
}

func TestValidateWorkflowAcceptsNonRequiredInputWithDefault(t *testing.T) {
	req := dispatchInputsWorkflowFixture()
	req.DispatchInputs = []DispatchInputSpec{
		{Name: CanonicalGitRefInput, Required: true, Default: CanonicalGitRefDefault},
		{Name: "extras_ref", Default: "support"},
	}
	req.Phases[0].Jobs[0].ExtraCheckouts = []RunnerCheckoutSpec{
		{Repo: "owner/extras", Ref: "${{ inputs.extras_ref }}", Path: "/workspace/extras"},
	}
	if err := ValidateWorkflowRegister(req); err != nil {
		t.Fatalf("ValidateWorkflowRegister: %v", err)
	}
}
