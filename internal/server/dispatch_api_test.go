package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/romaine-life/glimmung/internal/auth"
	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
	"github.com/romaine-life/glimmung/internal/domain/budget"
)

type fakeDispatchStore struct {
	fakeReadStore

	githubRepo    string
	githubRepoErr error
	project       *Project

	issue    *IssueDispatchData
	issueErr error

	wf           *Workflow
	wfErr        error
	workflows    []Workflow
	workflowsErr error

	lockErr      error
	lockReleased bool
	lockTTL      int

	run    *CreatedRun
	runErr error
	runReq *CreateRunRequest

	leaseResult Lease
	leaseErr    error
	leaseReq    *LeaseAcquireRequest
	startReq    *StartRunCycleRequest

	abortReason string
}

type patchingDispatchStore struct {
	*fakeDispatchStore
	patchedLeasePayload map[string]any
	patchErr            error
}

func (s *patchingDispatchStore) PatchLeasePayload(_ context.Context, project, id string, mutate func(payload map[string]any) error) error {
	if s.patchErr != nil {
		return s.patchErr
	}
	if project != s.leaseResult.Project || id != s.leaseResult.ID {
		return errors.New("unexpected lease patch target")
	}
	payload := map[string]any{
		"metadata": mapOrEmpty(s.leaseResult.Metadata),
		"state":    s.leaseResult.State,
	}
	if err := mutate(payload); err != nil {
		return err
	}
	s.patchedLeasePayload = payload
	if metadata := anyMap(payload["metadata"]); len(metadata) > 0 {
		s.leaseResult.Metadata = metadata
	}
	return nil
}

type fakeNativeLauncher struct {
	called        bool
	req           NativeLaunchRequest
	err           error
	ctxErrOnEntry error
}

func (l *fakeNativeLauncher) LaunchNativePhase(ctx context.Context, req NativeLaunchRequest) ([]string, error) {
	l.called = true
	l.req = req
	l.ctxErrOnEntry = ctx.Err()
	if l.err != nil {
		return nil, l.err
	}
	return []string{"native-job"}, nil
}

func (s *fakeDispatchStore) ReadProjectGitHubRepo(context.Context, string) (string, error) {
	return s.githubRepo, s.githubRepoErr
}

func (s *fakeDispatchStore) ReadProjectForDispatch(_ context.Context, project string) (Project, error) {
	if s.project != nil {
		return *s.project, nil
	}
	if s.githubRepoErr != nil {
		return Project{}, s.githubRepoErr
	}
	if s.githubRepo == "" {
		return Project{}, ErrNotFound
	}
	return Project{Name: project, GitHubRepo: s.githubRepo, Metadata: map[string]any{}}, nil
}

func (s *fakeDispatchStore) ReadIssueForDispatch(context.Context, string, int) (IssueDispatchData, error) {
	if s.issueErr != nil {
		return IssueDispatchData{}, s.issueErr
	}
	if s.issue == nil {
		return IssueDispatchData{}, ErrNotFound
	}
	return *s.issue, nil
}

func (s *fakeDispatchStore) GetWorkflowByName(context.Context, string, string) (*Workflow, error) {
	return s.wf, s.wfErr
}

func (s *fakeDispatchStore) ListProjectWorkflows(context.Context, string) ([]Workflow, error) {
	return s.workflows, s.workflowsErr
}

func (s *fakeDispatchStore) ClaimIssueLock(_ context.Context, _ string, _ int, _ string, ttlSeconds int) error {
	s.lockTTL = ttlSeconds
	return s.lockErr
}

func (s *fakeDispatchStore) ReleaseIssueLock(context.Context, string, int, string) {
	s.lockReleased = true
}

func (s *fakeDispatchStore) CreateRun(_ context.Context, req CreateRunRequest) (CreatedRun, error) {
	s.runReq = &req
	if s.runErr != nil {
		return CreatedRun{}, s.runErr
	}
	if s.run != nil {
		return *s.run, nil
	}
	return CreatedRun{ID: "run-1", RunNumber: 1, CycleNumber: 1, RunCycle: 1, RunDisplay: "1.1", CallbackToken: "tok"}, nil
}

func (s *fakeDispatchStore) StartRunCycle(_ context.Context, req StartRunCycleRequest) (int, error) {
	s.startReq = &req
	return 0, nil
}

func (s *fakeDispatchStore) AcquireLease(_ context.Context, req LeaseAcquireRequest) (Lease, error) {
	s.leaseReq = &req
	return s.leaseResult, s.leaseErr
}

func (s *fakeDispatchStore) ReadLeaseByRef(context.Context, string, string) (Lease, error) {
	return s.leaseResult, s.leaseErr
}

func (s *fakeDispatchStore) CancelLeaseByRef(context.Context, string, string) (CancelLeaseResult, error) {
	return CancelLeaseResult{}, nil
}

func (s *fakeDispatchStore) AbortRunByID(context.Context, string, string, string) (AbortRunResult, error) {
	return AbortRunResult{}, nil
}

func newDispatchTestHandler(store ReadStore, nativeLauncher NativeLauncher) http.Handler {
	adminAuthenticator := fakeAdminAuthenticator{user: auth.User{Sub: "admin"}}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/runs/dispatch", requireAdmin(adminAuthenticator, http.HandlerFunc(dispatchRunHandler(Settings{}, store, nativeLauncher))))
	return mux
}

// gatedTestPhases builds the minimum phase chain that passes the post-
// migration validation (prepare → verify → cleanup_early → touchpoint →
// touchpoint_gate → cleanup_final, with the pr_touchpoint primitive in
// the touchpoint phase and pr_merge in the gate). Tests that exercise
// dispatch flow against an in-memory workflow use this.
func gatedTestPhases() []PhaseSpec {
	return []PhaseSpec{
		{Name: "prepare", Kind: "k8s_job", WorkflowFilename: "k8s_job:prepare", Outputs: []string{IssueContractOutputKey}, Jobs: []NativeJobSpec{{ID: IssueContractJobID, Image: "runner:latest"}}},
		{Name: "verify", Kind: "k8s_job", WorkflowFilename: "k8s_job:verify", DependsOn: []string{"prepare"}, Verify: true, Jobs: []NativeJobSpec{{ID: "verify", Image: "runner:latest"}}},
		{Name: "cleanup_early", Kind: "k8s_job", WorkflowFilename: "k8s_job:cleanup_early", DependsOn: []string{"verify"}, RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, SkipWhenPreserveTestEnv: true, Jobs: []NativeJobSpec{{ID: "cleanup", Image: "runner:latest"}}},
		{Name: "touchpoint", Kind: "k8s_job", WorkflowFilename: "k8s_job:touchpoint", DependsOn: []string{"cleanup_early"}, RunOn: PhaseRunOnSuccess, Purpose: PhasePurposeReviewTouchpoint, Jobs: []NativeJobSpec{{ID: PRTouchpointJobID, Primitive: JobPrimitivePRTouchpoint, Managed: true}}},
		{Name: "touchpoint_gate", Kind: "k8s_job", WorkflowFilename: "k8s_job:touchpoint_gate", Purpose: PhasePurposeReviewGate, DependsOn: []string{"touchpoint"}, Jobs: []NativeJobSpec{{ID: PRMergeJobID, Primitive: JobPrimitivePRMerge, Managed: true}}},
		{Name: "cleanup_final", Kind: "k8s_job", WorkflowFilename: "k8s_job:cleanup_final", DependsOn: []string{"touchpoint_gate"}, RunOn: PhaseRunOnAlways, Purpose: PhasePurposeTeardown, Jobs: []NativeJobSpec{{ID: "cleanup-final", Image: "runner:latest"}}},
	}
}

func minimalDispatchStore() *fakeDispatchStore {
	leaseNum := 1
	wf := &Workflow{
		Name:                "main",
		Project:             "proj",
		Budget:              budget.Config{Total: 25},
		Phases:              gatedTestPhases(),
		DefaultRequirements: map[string]any{},
		Metadata:            map[string]any{},
	}
	return &fakeDispatchStore{
		githubRepo: "owner/repo",
		issue: &IssueDispatchData{
			ID:    "issue-1",
			Title: "Test issue",
			Body:  "body",
		},
		wf:        wf,
		workflows: []Workflow{*wf},
		leaseResult: Lease{
			ID:          "lease-1",
			Project:     "proj",
			LeaseNumber: &leaseNum,
			Host:        stringPtr("native-k8s"),
			State:       "claimed",
			Metadata: map[string]any{
				"native_k8s":           true,
				"native_slot_index":    "1",
				"native_slot_name":     "proj-1",
				"lease_callback_token": "lctok",
			},
		},
	}
}

func dispatchRequest(project string, issueNumber int) *http.Request {
	body, _ := json.Marshal(DispatchRunRequest{Project: project, IssueNumber: issueNumber})
	return httptest.NewRequest(http.MethodPost, "/v1/runs/dispatch", bytes.NewReader(body))
}

func readDispatchResult(t *testing.T, rec *httptest.ResponseRecorder) PublicDispatchResult {
	t.Helper()
	var result PublicDispatchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDispatchRunMissingProject(t *testing.T) {
	store := minimalDispatchStore()
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(DispatchRunRequest{IssueNumber: 1})
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/dispatch", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDispatchRunProjectNotFound(t *testing.T) {
	store := minimalDispatchStore()
	store.githubRepoErr = ErrNotFound
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_project" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunNoWorkflowRegistered(t *testing.T) {
	store := minimalDispatchStore()
	store.wf = nil
	store.workflows = nil
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_workflow" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunRejectsWorkflowWithoutIssueContract(t *testing.T) {
	store := minimalDispatchStore()
	store.wf.Phases[0].Outputs = nil
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), IssueContractOutputKey) {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if store.runReq != nil || store.leaseReq != nil {
		t.Fatalf("invalid workflow should fail before creating run or lease: run=%#v lease=%#v", store.runReq, store.leaseReq)
	}
}

func TestDispatchRunAlreadyRunning(t *testing.T) {
	store := minimalDispatchStore()
	store.lockErr = &AlreadyRunningError{HeldBy: "holder-123", ExpiresAt: time.Now().Add(time.Hour)}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "already_running" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunAlreadyRunningErrIs(t *testing.T) {
	err := &AlreadyRunningError{HeldBy: "x", ExpiresAt: time.Now()}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("errors.Is should match ErrAlreadyRunning")
	}
}

func TestDispatchRunRequiresNativeLauncher(t *testing.T) {
	store := minimalDispatchStore()
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq != nil || store.leaseReq != nil {
		t.Fatalf("request should fail before creating run or lease")
	}
}

func TestDispatchRunDispatchedNativeK8sJob(t *testing.T) {
	store := minimalDispatchStore()
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readDispatchResult(t, rec)
	if result.State != "dispatched" {
		t.Fatalf("state=%q", result.State)
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	if launcher.req.Phase.Name != "prepare" || launcher.req.Run.ID != "run-1" {
		t.Fatalf("launch request=%#v", launcher.req)
	}
	if store.runReq == nil || store.runReq.InitialPhaseKind != "k8s_job" {
		t.Fatalf("run request=%#v", store.runReq)
	}
	if store.runReq.SlotLeaseRef == "" || store.startReq == nil || store.startReq.SlotLeaseRef != store.runReq.SlotLeaseRef {
		t.Fatalf("lease should be attached before run admission: run=%#v start=%#v", store.runReq, store.startReq)
	}
	if store.leaseReq == nil || store.leaseReq.Metadata["native_k8s"] != true {
		t.Fatalf("lease request=%#v", store.leaseReq)
	}
	wantTTL := nativeRunLeaseTTLSeconds(store.wf)
	if wantTTL <= 900 {
		t.Fatalf("test fixture ttl=%d, want larger than retired 15-minute default", wantTTL)
	}
	if store.lockTTL != wantTTL {
		t.Fatalf("issue lock ttl=%d, want workflow ttl=%d", store.lockTTL, wantTTL)
	}
	if store.leaseReq.TTLSeconds == nil || *store.leaseReq.TTLSeconds != wantTTL {
		t.Fatalf("lease ttl=%v, want %d", store.leaseReq.TTLSeconds, wantTTL)
	}
}

func TestDispatchRunPersistsPostRunWorkContextOnPreclaimedLease(t *testing.T) {
	base := minimalDispatchStore()
	base.leaseResult.Metadata["work_context_branch"] = "issue-168-run-unknown"
	store := &patchingDispatchStore{fakeDispatchStore: base}
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 168))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	if got := base.leaseReq.Metadata["work_context_branch"]; got != nil {
		t.Fatalf("pre-run lease request should not stamp a provisional branch, got %#v", got)
	}
	if store.patchedLeasePayload == nil {
		t.Fatal("lease metadata was not persisted after run creation")
	}
	patched := anyMap(store.patchedLeasePayload["metadata"])
	if got, want := patched["work_context_id"], "run-1"; got != want {
		t.Fatalf("patched work_context_id=%#v, want %q", got, want)
	}
	if got, want := patched["work_context_branch"], "glimmung/run-1"; got != want {
		t.Fatalf("patched work_context_branch=%#v, want %q", got, want)
	}
	if got, want := launcher.req.Lease.Metadata["work_context_branch"], "glimmung/run-1"; got != want {
		t.Fatalf("launch work_context_branch=%#v, want %q", got, want)
	}
}

func TestRunCycleLeaseMetadataUsesPlaybookWorkContextBranch(t *testing.T) {
	run := RunReplayData{
		ID:          "run-1",
		Project:     "proj",
		IssueNumber: 168,
		TriggerSource: map[string]any{
			"kind": "playbook",
			"work_context": map[string]any{
				"id":       "playbook:pb-1:entry-1",
				"branch":   "glimmung/playbooks/pb-1/entry-1",
				"base_ref": "main",
				"state":    "in_use",
			},
		},
	}
	metadata := runCycleLeaseMetadata(run, IssueDispatchData{Title: "issue"}, "owner/repo", "prepare", 0, nil)
	if got, want := metadata["work_context_branch"], "glimmung/playbooks/pb-1/entry-1"; got != want {
		t.Fatalf("work_context_branch=%#v, want %q", got, want)
	}
	if got, want := metadata["work_context_base_ref"], "main"; got != want {
		t.Fatalf("work_context_base_ref=%#v, want %q", got, want)
	}
	if got := metadata["work_context_id"]; got != nil {
		t.Fatalf("explicit playbook branch should not be replaced by default run id, got work_context_id=%#v", got)
	}
}

func TestDispatchRunSnapshotsAgentRuntimePolicy(t *testing.T) {
	store := minimalDispatchStore()
	store.project = &Project{
		Name:            "proj",
		GitHubRepo:      "owner/repo",
		ConfigSchemaRef: "project-config:abc123",
		Metadata: map[string]any{
			"agent_runtime": agentruntime.Config{
				Profiles: map[string]agentruntime.Profile{
					"project-fast": {ID: "project-fast", Provider: agentruntime.ProviderCodex, Model: "gpt-5.4-mini", ReasoningEffort: "medium"},
					"issue-deep":   {ID: "issue-deep", Provider: agentruntime.ProviderCodex, Model: "gpt-5.5", ReasoningEffort: "xhigh"},
				},
				Policy: agentruntime.Policy{
					Default: agentruntime.PolicyDecision{Mode: agentruntime.ModeOverride, Profile: "project-fast"},
				},
			},
		},
	}
	store.issue.Agent = &agentruntime.Policy{
		Default: agentruntime.PolicyDecision{Mode: agentruntime.ModeOverride, Profile: "issue-deep"},
		Slots: map[string]agentruntime.PolicyDecision{
			"implementation": {Mode: agentruntime.ModeOverride, Profile: "project-fast"},
		},
	}
	store.wf.Phases[0].Jobs[0].Managed = true
	store.wf.Phases[0].Jobs[0].Steps = []NativeStepSpec{{
		Slug:  "implement",
		Type:  "agent",
		Agent: &AgentStepSpec{Slot: "implementation"},
	}}

	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq == nil {
		t.Fatal("run request was not captured")
	}
	if got := store.runReq.AgentRuntime.Default.ProfileID; got != "issue-deep" {
		t.Fatalf("default profile=%q, want issue-deep", got)
	}
	if got := store.runReq.AgentRuntime.Default.Source; got != "issue" {
		t.Fatalf("default source=%q, want issue", got)
	}
	slot, ok := store.runReq.AgentRuntime.Slots["implementation"]
	if !ok {
		t.Fatalf("implementation slot missing from snapshot: %#v", store.runReq.AgentRuntime.Slots)
	}
	if slot.ProfileID != "project-fast" || slot.Source != "issue" {
		t.Fatalf("implementation slot=%#v, want project-fast from issue", slot)
	}
	if got := store.runReq.AgentRuntime.ProjectConfigSchemaRef; got != "project-config:abc123" {
		t.Fatalf("project schema ref=%q", got)
	}
	if store.leaseReq == nil {
		t.Fatal("lease request was not captured")
	}
	leaseSnapshot, ok := store.leaseReq.Metadata["agent_runtime"].(agentruntime.Snapshot)
	if !ok {
		t.Fatalf("lease agent_runtime=%T %#v", store.leaseReq.Metadata["agent_runtime"], store.leaseReq.Metadata["agent_runtime"])
	}
	if leaseSnapshot.Default.ProfileID != "issue-deep" {
		t.Fatalf("lease default profile=%q", leaseSnapshot.Default.ProfileID)
	}
	if launcher.req.Run.AgentRuntime.Default.ProfileID != "issue-deep" {
		t.Fatalf("launch snapshot=%#v", launcher.req.Run.AgentRuntime)
	}
}

func TestAdmitRunCycleAcquiresWorkflowTTLForQueuedRunWithoutLease(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseReq = nil
	launcher := &fakeNativeLauncher{}
	callbackToken := "queued-token"
	runNumber := 1
	cycleNumber := 1
	runCycleNumber := 1
	runDisplayNumber := "1.1"
	run := RunReplayData{
		ID:               "queued-run",
		Project:          "proj",
		WorkflowName:     store.wf.Name,
		IssueNumber:      1,
		IssueRepo:        store.githubRepo,
		CallbackToken:    &callbackToken,
		RunNumber:        &runNumber,
		CycleNumber:      &cycleNumber,
		RunCycleNumber:   &runCycleNumber,
		RunDisplayNumber: &runDisplayNumber,
		TriggerSource:    map[string]any{"kind": "dispatch"},
	}

	admission, err := admitRunCycle(
		context.Background(),
		store,
		launcher,
		run,
		store.wf,
		*store.issue,
		store.githubRepo,
		LeasePurposeDispatch,
	)
	if err != nil {
		t.Fatalf("admitRunCycle: %v", err)
	}
	if admission.State != "dispatched" {
		t.Fatalf("state=%q detail=%v", admission.State, admission.Detail)
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	wantTTL := nativeRunLeaseTTLSeconds(store.wf)
	if store.leaseReq == nil || store.leaseReq.TTLSeconds == nil || *store.leaseReq.TTLSeconds != wantTTL {
		t.Fatalf("lease ttl=%#v, want %d", store.leaseReq, wantTTL)
	}
}

func TestDispatchRunSnapshotsVideoEvidenceRequirementFromIssueLabel(t *testing.T) {
	store := minimalDispatchStore()
	store.issue.Labels = []string{"evidence:video"}
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.runReq == nil || len(store.runReq.EvidenceRequirements) != 1 {
		t.Fatalf("run request=%#v", store.runReq)
	}
	if store.runReq.EvidenceRequirements[0].Kind != "video" {
		t.Fatalf("evidence requirements=%#v", store.runReq.EvidenceRequirements)
	}
	if store.leaseReq == nil || store.leaseReq.Metadata["evidence_requirements"] == nil {
		t.Fatalf("lease metadata=%#v", store.leaseReq)
	}
	if len(launcher.req.Run.EvidenceRequirements) != 1 || launcher.req.Run.EvidenceRequirements[0].Kind != "video" {
		t.Fatalf("launch run=%#v", launcher.req.Run)
	}
}

func TestDispatchRunLaunchUsesPostCommitContext(t *testing.T) {
	store := minimalDispatchStore()
	launcher := &fakeNativeLauncher{}
	rec := httptest.NewRecorder()
	req := dispatchRequest("proj", 1)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	newDispatchTestHandler(store, launcher).ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !launcher.called {
		t.Fatal("native launcher was not called")
	}
	if launcher.ctxErrOnEntry != nil {
		t.Fatalf("launch context err=%v, want nil", launcher.ctxErrOnEntry)
	}
}

func TestDispatchRunNoCapacity(t *testing.T) {
	store := minimalDispatchStore()
	store.leaseErr = ErrUnavailable
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_capacity" {
		t.Fatalf("state=%q", got)
	}
	if store.runReq != nil || store.startReq != nil {
		t.Fatalf("no-capacity dispatch should not create or start a run: run=%#v start=%#v", store.runReq, store.startReq)
	}
	if !store.lockReleased {
		t.Fatal("expected issue lock release after no-capacity dispatch")
	}
}

func TestDispatchRunNativeDispatchFailed(t *testing.T) {
	store := minimalDispatchStore()
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{err: errors.New("kube unavailable")}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := readDispatchResult(t, rec)
	if result.State != "dispatch_failed" || result.Detail == nil {
		t.Fatalf("result=%#v", result)
	}
}

func TestDispatchRunCreateRunFailReleasesLock(t *testing.T) {
	store := minimalDispatchStore()
	store.runErr = errors.New("store unavailable")
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !store.lockReleased {
		t.Fatal("expected ReleaseIssueLock after CreateRun failure")
	}
}

func TestDispatchRunMultipleWorkflowsRequiresName(t *testing.T) {
	store := minimalDispatchStore()
	phases := store.wf.Phases
	store.wf = nil
	store.workflows = []Workflow{
		{Name: "wf-a", Project: "proj", Phases: phases, Budget: budget.Config{Total: 25}},
		{Name: "wf-b", Project: "proj", Phases: phases, Budget: budget.Config{Total: 25}},
	}
	rec := httptest.NewRecorder()
	newDispatchTestHandler(store, nil).ServeHTTP(rec, dispatchRequest("proj", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "no_workflow" {
		t.Fatalf("state=%q", got)
	}
}

func TestDispatchRunWorkflowAlias(t *testing.T) {
	store := minimalDispatchStore()
	store.workflows = []Workflow{
		{Name: "other", Project: "proj", Phases: store.wf.Phases, Budget: budget.Config{Total: 25}},
		*store.wf,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/dispatch", bytes.NewBufferString(`{"project":"proj","issue_number":1,"workflow":"main"}`))
	newDispatchTestHandler(store, &fakeNativeLauncher{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readDispatchResult(t, rec).State; got != "dispatched" {
		t.Fatalf("state=%q", got)
	}
}
