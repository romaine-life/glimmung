package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
	"github.com/romaine-life/glimmung/internal/domain/budget"
	"github.com/romaine-life/glimmung/internal/domain/publicids"
	"github.com/romaine-life/glimmung/internal/domain/whenexpr"
	"github.com/romaine-life/glimmung/internal/metrics"
)

type SyntheticDispatchRequest struct {
	Project              string                         `json:"project"`
	IssueNumber          int                            `json:"issue_number"`
	WorkflowName         string                         `json:"workflow_name"`
	Workflow             string                         `json:"workflow"`
	StartAtPhase         string                         `json:"start_at_phase"`
	CopyPhaseOutputsFrom *SyntheticCopyPhaseOutputsFrom `json:"copy_phase_outputs_from,omitempty"`
	SuppliedPhaseOutputs []SyntheticSuppliedPhaseOutput `json:"supplied_phase_outputs"`
	ExecutionContext     SyntheticExecutionContext      `json:"execution_context"`
	Reason               string                         `json:"reason"`
	TriggerSource        map[string]any                 `json:"trigger_source"`
}

type SyntheticCopyPhaseOutputsFrom struct {
	Run    string              `json:"run"`
	Phases map[string][]string `json:"phases"`
}

type SyntheticSuppliedPhaseOutput struct {
	Phase        string            `json:"phase"`
	PhaseOutputs map[string]string `json:"phase_outputs"`
}

type SyntheticExecutionContext struct {
	SlotLeaseRef  string `json:"slot_lease_ref"`
	Namespace     string `json:"namespace"`
	ValidationURL string `json:"validation_url"`
}

type SyntheticDispatchCopyStore interface {
	ReadRunIDForNumber(ctx context.Context, project string, issueNumber int, runNumber string) (string, string, error)
	ReadRunForReplay(ctx context.Context, project, runID string) (RunReplayData, error)
}

func syntheticDispatchRunHandler(settings Settings, store ReadStore, nativeLauncher NativeLauncher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dispatchStore, ok := store.(RunDispatchStore)
		if !ok || dispatchStore == nil {
			writeProblem(w, http.StatusServiceUnavailable, "dispatch store not configured")
			return
		}
		var req SyntheticDispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid request body")
			return
		}
		globalRuntime, err := validateAgentRuntimeConfigForSettings(settings)
		if err != nil {
			writeUnavailable(w, r, "global agent runtime config is invalid", "invalid_agent_runtime_config")
			return
		}
		result, problem := syntheticDispatchRunWithAgentRuntime(r.Context(), dispatchStore, nativeLauncher, globalRuntime, req)
		if problem != nil {
			writeProblem(w, problem.status, problem.message)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func syntheticDispatchRunWithAgentRuntime(ctx context.Context, store RunDispatchStore, nativeLauncher NativeLauncher, globalRuntime agentruntime.Config, req SyntheticDispatchRequest) (PublicDispatchResult, *dispatchProblem) {
	req.Project = strings.TrimSpace(req.Project)
	req.WorkflowName = strings.TrimSpace(firstNonEmpty(req.WorkflowName, req.Workflow))
	req.StartAtPhase = strings.TrimSpace(req.StartAtPhase)
	req.Reason = strings.TrimSpace(req.Reason)
	req.ExecutionContext.SlotLeaseRef = strings.TrimSpace(req.ExecutionContext.SlotLeaseRef)
	req.ExecutionContext.Namespace = strings.TrimSpace(req.ExecutionContext.Namespace)
	req.ExecutionContext.ValidationURL = strings.TrimSpace(req.ExecutionContext.ValidationURL)

	if req.Project == "" {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusBadRequest, message: "project required"}
	}
	if req.IssueNumber <= 0 {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusBadRequest, message: "issue_number required"}
	}
	if req.StartAtPhase == "" {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusBadRequest, message: "start_at_phase required"}
	}
	if req.Reason == "" {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusBadRequest, message: "reason required"}
	}
	if req.ExecutionContext.SlotLeaseRef == "" {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "execution_context.slot_lease_ref required for synthetic native dispatch"}
	}
	if nativeLauncher == nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusServiceUnavailable, message: "native launcher not configured"}
	}

	project, err := store.ReadProjectForDispatch(ctx, req.Project)
	if errors.Is(err, ErrNotFound) {
		return PublicDispatchResult{State: "no_project", Detail: stringPtr(fmt.Sprintf("project %q not registered", req.Project))}, nil
	}
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read project failed"}
	}
	issueRepo := project.GitHubRepo
	issue, err := store.ReadIssueForDispatch(ctx, req.Project, req.IssueNumber)
	if errors.Is(err, ErrNotFound) {
		return PublicDispatchResult{State: "no_project", Detail: stringPtr(fmt.Sprintf("no issue %s#%d", req.Project, req.IssueNumber))}, nil
	}
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read issue failed"}
	}
	wf, resolveDetail, err := resolveDispatchWorkflow(ctx, store, req.Project, req.WorkflowName)
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read workflow failed"}
	}
	if wf == nil {
		return PublicDispatchResult{State: "no_workflow", Detail: &resolveDetail}, nil
	}
	if err := ValidateWorkflowRegister(WorkflowRegister{
		Project:             wf.Project,
		Name:                wf.Name,
		Phases:              wf.Phases,
		PR:                  wf.PR,
		Budget:              wf.Budget,
		Constraints:         wf.Constraints,
		DefaultRequirements: wf.DefaultRequirements,
		Vars:                wf.Vars,
		DispatchInputs:      wf.DispatchInputs,
		Metadata:            wf.Metadata,
	}); err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: err.Error()}
	}
	// Resolve dispatch inputs exactly as the ordinary dispatch path does: apply
	// the workflow's declared defaults (e.g. git_ref -> main) and reject a
	// genuinely-missing required input here, before any run row or lease/lock is
	// created. Synthetic dispatch carries no caller-supplied inputs today, so
	// this resolves to the declared defaults; running it still matters so the
	// run's RunInputs satisfy every `${{ inputs.X }}` phase template (e.g. a
	// phase checkout.ref of `${{ inputs.git_ref }}`) at render time.
	resolvedInputs, inputErr := resolveDispatchRunInputs(wf.DispatchInputs, nil)
	if inputErr != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: inputErr.Error()}
	}
	startPhase, startIndex := phaseWithIndex(wf.Phases, req.StartAtPhase)
	if startPhase == nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("start_at_phase %q is not registered on workflow %q", req.StartAtPhase, wf.Name)}
	}
	phaseKind := workflowPhaseKind(startPhase.Kind)
	if err := validateNativeWorkflowKind(phaseKind); err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: err.Error()}
	}
	copied, copyProvenance, problem := syntheticCopiedPhaseOutputs(ctx, store, req, wf, startIndex)
	if problem != nil {
		return PublicDispatchResult{}, problem
	}
	suppliedInputs, problem := mergeSyntheticSuppliedPhaseOutputs(copied, req.SuppliedPhaseOutputs)
	if problem != nil {
		return PublicDispatchResult{}, problem
	}
	suppliedAttempts, problem := syntheticSuppliedAttempts(suppliedInputs, wf, startIndex)
	if problem != nil {
		return PublicDispatchResult{}, problem
	}
	phaseInputs, err := substituteCompletionPhaseInputs(*startPhase, RunReplayData{Attempts: suppliedAttempts})
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "start_at_phase inputs are not satisfied by supplied_phase_outputs: " + err.Error()}
	}
	lease, err := store.ReadLeaseByRef(ctx, req.Project, req.ExecutionContext.SlotLeaseRef)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "execution_context.slot_lease_ref does not identify a claimed lease"}
		}
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "read slot lease failed"}
	}
	if lease.State != "claimed" {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "execution_context.slot_lease_ref is not claimed"}
	}

	projectRuntime, projectHasRuntime, err := agentruntime.ConfigFromMetadata(project.Metadata)
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "project agent runtime config is invalid: " + err.Error()}
	}
	agentRuntime, err := agentruntime.Resolve(globalRuntime, projectRuntime, projectHasRuntime, issue.Agent, workflowAgentSlots(*wf), project.ConfigSchemaRef)
	if err != nil {
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "resolve agent runtime: " + err.Error()}
	}

	holderID := newDispatchID()
	leaseTTLSeconds := nativeRunLeaseTTLSeconds(wf)
	if err := store.ClaimIssueLock(ctx, req.Project, req.IssueNumber, holderID, leaseTTLSeconds); err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			return PublicDispatchResult{State: "already_running", Workflow: &wf.Name, Detail: stringPtr(err.Error())}, nil
		}
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "claim issue lock failed"}
	}
	var wfBudget *budget.Config
	if wf.Budget.Total > 0 {
		c := wf.Budget
		wfBudget = &c
	}
	resolvedBudget := budget.ResolveBudget(issue.Labels, wfBudget)
	triggerSource := syntheticTriggerSource(req, copyProvenance)
	evidenceRequirements := dispatchEvidenceRequirements(wf, issue)
	workflowFilename := firstNonEmpty(startPhase.WorkflowFilename, fmt.Sprintf("%s:%s", phaseKind, startPhase.Name))
	run, err := store.CreateRun(ctx, CreateRunRequest{
		Project:                 req.Project,
		Workflow:                wf.Name,
		WorkflowSchemaRef:       wf.SchemaRef,
		IssueID:                 issue.ID,
		IssueRepo:               issueRepo,
		IssueNumber:             req.IssueNumber,
		Budget:                  resolvedBudget,
		InitialPhaseName:        startPhase.Name,
		InitialPhaseKind:        phaseKind,
		InitialWorkflowFilename: workflowFilename,
		IssueLockHolderID:       holderID,
		SlotLeaseRef:            req.ExecutionContext.SlotLeaseRef,
		EntrypointPhase:         startPhase.Name,
		SuppliedAttempts:        suppliedAttempts,
		ValidationURL:           req.ExecutionContext.ValidationURL,
		TriggerSource:           triggerSource,
		RunInputs:               resolvedInputs,
		EvidenceRequirements:    evidenceRequirements,
		AgentRuntime:            agentRuntime,
		PreserveTestEnv:         issue.PreserveTestEnv,
	})
	if err != nil {
		store.ReleaseIssueLock(ctx, req.Project, req.IssueNumber, holderID)
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "create run failed"}
	}
	metrics.RecordRunCreated(wf.Name)
	runData := RunReplayData{
		ID:                   run.ID,
		Project:              req.Project,
		WorkflowName:         wf.Name,
		WorkflowSchemaRef:    wf.SchemaRef,
		Budget:               resolvedBudget,
		IssueNumber:          req.IssueNumber,
		RunNumber:            &run.RunNumber,
		CycleNumber:          &run.CycleNumber,
		RunCycleNumber:       &run.RunCycle,
		RunDisplayNumber:     &run.RunDisplay,
		IssueRepo:            issueRepo,
		CallbackToken:        &run.CallbackToken,
		IssueLockHolderID:    &holderID,
		SlotLeaseRef:         &req.ExecutionContext.SlotLeaseRef,
		EntrypointPhase:      &startPhase.Name,
		TriggerSource:        triggerSource,
		RunInputs:            resolvedInputs,
		EvidenceRequirements: evidenceRequirements,
		AgentRuntime:         agentRuntime,
		PreserveTestEnv:      issue.PreserveTestEnv,
		Attempts:             append(append([]RunAttemptData{}, suppliedAttempts...), RunAttemptData{AttemptIndex: len(suppliedAttempts), Phase: startPhase.Name}),
	}
	metadata := runCycleLeaseMetadata(runData, issue, issueRepo, startPhase.Name, len(suppliedAttempts), phaseInputs)
	if req.ExecutionContext.Namespace != "" {
		metadata["synthetic_namespace"] = req.ExecutionContext.Namespace
	}
	merged := mapOrEmpty(lease.Metadata)
	for key, value := range metadata {
		merged[key] = value
	}
	lease.Metadata = merged
	if err := persistLeaseMetadata(ctx, store, lease, merged); err != nil {
		_, _ = store.AbortRunByID(ctx, req.Project, run.ID, "synthetic_lease_metadata_failed: "+err.Error())
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "persist lease metadata failed"}
	}
	attemptIdx, err := store.StartRunCycle(ctx, StartRunCycleRequest{
		Project:          req.Project,
		RunID:            run.ID,
		PhaseName:        startPhase.Name,
		PhaseKind:        phaseKind,
		WorkflowFilename: workflowFilename,
		SlotLeaseRef:     req.ExecutionContext.SlotLeaseRef,
	})
	if err != nil {
		_, _ = store.AbortRunByID(ctx, req.Project, run.ID, "start_cycle_failed: "+err.Error())
		return PublicDispatchResult{}, &dispatchProblem{status: http.StatusInternalServerError, message: "start run cycle failed"}
	}
	runData.Attempts[len(runData.Attempts)-1].AttemptIndex = attemptIdx
	// Job-level when conditions apply to synthetic dispatches exactly as to
	// ordinary ones: break-glass runs still honor the registered shape.
	_, _, skippedJobs, err := evaluatePhaseWhen(*startPhase, whenexpr.Context{
		Vars:            wf.Vars,
		Inputs:          runData.RunInputs,
		PreserveTestEnv: runData.PreserveTestEnv,
	})
	if err != nil {
		_, _ = store.AbortRunByID(ctx, req.Project, run.ID, "native_dispatch_failed: "+err.Error())
		return PublicDispatchResult{State: "dispatch_failed", RunNumber: &run.RunNumber, CycleNumber: &run.CycleNumber, RunCycle: &run.RunCycle, RunID: &run.ID, RunRef: stringPtr(publicids.RunRef(req.Project, req.IssueNumber, run.RunDisplay)), Workflow: &wf.Name, Detail: stringPtr("native dispatch failed: " + err.Error())}, nil
	}
	if len(skippedJobs) > 0 {
		if err := store.RecordNativeJobsSkipped(ctx, req.Project, run.ID, startPhase.Name, skippedJobs); err != nil {
			_, _ = store.AbortRunByID(ctx, req.Project, run.ID, "native_dispatch_failed: "+err.Error())
			return PublicDispatchResult{State: "dispatch_failed", RunNumber: &run.RunNumber, CycleNumber: &run.CycleNumber, RunCycle: &run.RunCycle, RunID: &run.ID, RunRef: stringPtr(publicids.RunRef(req.Project, req.IssueNumber, run.RunDisplay)), Workflow: &wf.Name, Detail: stringPtr("native dispatch failed: " + err.Error())}, nil
		}
	}
	launched, err := launchCommittedNativePhase(ctx, nativeLauncher, NativeLaunchRequest{Lease: lease, Workflow: *wf, Phase: *startPhase, Run: runData, SkipJobIDs: skippedJobs})
	if err != nil {
		_, _ = store.AbortRunByID(ctx, req.Project, run.ID, "native_dispatch_failed: "+err.Error())
		return PublicDispatchResult{State: "dispatch_failed", RunNumber: &run.RunNumber, CycleNumber: &run.CycleNumber, RunCycle: &run.RunCycle, RunID: &run.ID, RunRef: stringPtr(publicids.RunRef(req.Project, req.IssueNumber, run.RunDisplay)), Workflow: &wf.Name, Detail: stringPtr("native dispatch failed: " + err.Error())}, nil
	}
	_ = recordLaunchedNativeJobs(ctx, store, runData, *startPhase, launched)
	runRef := publicids.RunRef(req.Project, req.IssueNumber, run.RunDisplay)
	return PublicDispatchResult{
		State:       "dispatched",
		IssueRef:    stringPtr(publicids.IssueRef(req.Project, &req.IssueNumber)),
		IssueNumber: &req.IssueNumber,
		RunNumber:   &run.RunNumber,
		CycleNumber: &run.CycleNumber,
		RunCycle:    &run.RunCycle,
		RunID:       &run.ID,
		RunRef:      &runRef,
		Workflow:    &wf.Name,
		Lease:       "claimed",
		Host:        lease.Host,
	}, nil
}

func syntheticCopiedPhaseOutputs(ctx context.Context, store RunDispatchStore, req SyntheticDispatchRequest, wf *Workflow, startIndex int) ([]SyntheticSuppliedPhaseOutput, []map[string]any, *dispatchProblem) {
	if req.CopyPhaseOutputsFrom == nil {
		return nil, nil, nil
	}
	copyReq := *req.CopyPhaseOutputsFrom
	copyReq.Run = strings.TrimSpace(copyReq.Run)
	if copyReq.Run == "" {
		return nil, nil, &dispatchProblem{status: http.StatusBadRequest, message: "copy_phase_outputs_from.run required"}
	}
	if len(copyReq.Phases) == 0 {
		return nil, nil, &dispatchProblem{status: http.StatusBadRequest, message: "copy_phase_outputs_from.phases required"}
	}
	copyStore, ok := store.(SyntheticDispatchCopyStore)
	if !ok || copyStore == nil {
		return nil, nil, &dispatchProblem{status: http.StatusServiceUnavailable, message: "synthetic phase-output copy store not configured"}
	}
	runID, sourceRunRef, err := copyStore.ReadRunIDForNumber(ctx, req.Project, req.IssueNumber, copyReq.Run)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "copy_phase_outputs_from.run not found"}
	}
	if err != nil {
		return nil, nil, &dispatchProblem{status: http.StatusInternalServerError, message: "read copy source run failed"}
	}
	sourceRun, err := copyStore.ReadRunForReplay(ctx, req.Project, runID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "copy_phase_outputs_from.run not found"}
	}
	if err != nil {
		return nil, nil, &dispatchProblem{status: http.StatusInternalServerError, message: "read copy source run failed"}
	}

	phaseIndex := map[string]int{}
	for i, phase := range wf.Phases {
		phaseIndex[phase.Name] = i
	}
	out := make([]SyntheticSuppliedPhaseOutput, 0, len(copyReq.Phases))
	provenance := make([]map[string]any, 0)
	seenPhase := map[string]bool{}
	for rawPhase, rawKeys := range copyReq.Phases {
		phase := strings.TrimSpace(rawPhase)
		if phase == "" {
			return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: "copy_phase_outputs_from.phases contains an empty phase"}
		}
		idx, ok := phaseIndex[phase]
		if !ok {
			return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("copy source phase %q is not registered on workflow %q", phase, wf.Name)}
		}
		if idx >= startIndex {
			return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("copy source phase %q must be before start_at_phase", phase)}
		}
		if seenPhase[phase] {
			return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("copy source phase %q is repeated", phase)}
		}
		seenPhase[phase] = true
		keys := cleanCopyOutputKeys(rawKeys)
		if len(keys) == 0 {
			return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("copy source phase %q has no output keys", phase)}
		}
		attempt := latestCompletedAttemptForPhase(sourceRun.Attempts, phase)
		if attempt == nil {
			return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("copy source run has no completed attempt for phase %q", phase)}
		}
		outputs := map[string]string{}
		for _, key := range keys {
			value, ok := attempt.PhaseOutputs[key]
			if !ok {
				return nil, nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("copy source phase %q is missing output %q", phase, key)}
			}
			outputs[key] = value
		}
		out = append(out, SyntheticSuppliedPhaseOutput{Phase: phase, PhaseOutputs: outputs})
		provenance = append(provenance, map[string]any{
			"source_run":           sourceRunRef,
			"source_run_id":        sourceRun.ID,
			"source_phase":         phase,
			"source_attempt_index": attempt.AttemptIndex,
			"target_phase":         phase,
			"keys":                 keys,
		})
	}
	return out, provenance, nil
}

func cleanCopyOutputKeys(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		key := strings.TrimSpace(value)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func latestCompletedAttemptForPhase(attempts []RunAttemptData, phase string) *RunAttemptData {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Phase == phase && attempts[i].Completed {
			return &attempts[i]
		}
	}
	return nil
}

func mergeSyntheticSuppliedPhaseOutputs(copied []SyntheticSuppliedPhaseOutput, supplied []SyntheticSuppliedPhaseOutput) ([]SyntheticSuppliedPhaseOutput, *dispatchProblem) {
	if len(copied) == 0 {
		return supplied, nil
	}
	merged := make([]SyntheticSuppliedPhaseOutput, 0, len(copied)+len(supplied))
	byPhase := map[string]int{}
	for _, input := range copied {
		phase := strings.TrimSpace(input.Phase)
		outputs := map[string]string{}
		for key, value := range input.PhaseOutputs {
			outputs[key] = value
		}
		byPhase[phase] = len(merged)
		merged = append(merged, SyntheticSuppliedPhaseOutput{Phase: phase, PhaseOutputs: outputs})
	}
	seenSupplied := map[string]bool{}
	for _, input := range supplied {
		phase := strings.TrimSpace(input.Phase)
		if seenSupplied[phase] {
			return nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("supplied phase %q is repeated", phase)}
		}
		seenSupplied[phase] = true
		if idx, ok := byPhase[phase]; ok {
			for key, value := range input.PhaseOutputs {
				if existing, exists := merged[idx].PhaseOutputs[key]; exists && existing != value {
					return nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("supplied phase %q output %q conflicts with copied output", phase, key)}
				}
				merged[idx].PhaseOutputs[key] = value
			}
			continue
		}
		byPhase[phase] = len(merged)
		merged = append(merged, input)
	}
	return merged, nil
}

func syntheticSuppliedAttempts(inputs []SyntheticSuppliedPhaseOutput, wf *Workflow, startIndex int) ([]RunAttemptData, *dispatchProblem) {
	if len(inputs) == 0 {
		return nil, nil
	}
	phaseIndex := map[string]int{}
	for i, phase := range wf.Phases {
		phaseIndex[phase.Name] = i
	}
	seen := map[string]bool{}
	out := make([]RunAttemptData, 0, len(inputs))
	for _, input := range inputs {
		phase := strings.TrimSpace(input.Phase)
		if phase == "" {
			return nil, &dispatchProblem{status: http.StatusBadRequest, message: "supplied_phase_outputs phase required"}
		}
		idx, ok := phaseIndex[phase]
		if !ok {
			return nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("supplied phase %q is not registered on workflow %q", phase, wf.Name)}
		}
		if idx >= startIndex {
			return nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("supplied phase %q must be before start_at_phase", phase)}
		}
		if seen[phase] {
			return nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("supplied phase %q is repeated", phase)}
		}
		seen[phase] = true
		outputs := map[string]string{}
		for key, value := range input.PhaseOutputs {
			trimmed := strings.TrimSpace(key)
			if trimmed == "" {
				return nil, &dispatchProblem{status: http.StatusUnprocessableEntity, message: fmt.Sprintf("supplied phase %q has an empty output key", phase)}
			}
			outputs[trimmed] = value
		}
		out = append(out, RunAttemptData{
			AttemptIndex: len(out),
			Phase:        phase,
			Conclusion:   "supplied",
			Verification: verificationDataFromPhaseOutputs(outputs),
			Decision:     "advance",
			Completed:    true,
			CarryForward: true,
			PhaseOutputs: outputs,
		})
	}
	return out, nil
}

func verificationDataFromPhaseOutputs(outputs map[string]string) *RunVerificationData {
	raw := strings.TrimSpace(outputs["verification"])
	if raw == "" {
		return nil
	}
	payload, ok := decodeEvidenceJSONOutputObject(raw)
	if !ok {
		return nil
	}
	verification := &RunVerificationData{
		Status:       strings.TrimSpace(stringValue(payload["status"])),
		Reasons:      stringSliceFromAny(payload["reasons"]),
		EvidenceRefs: stringSliceFromAny(payload["evidence_refs"]),
		Evidence:     EvidenceArtifactsFromVerificationPayload(payload),
	}
	verification.EvidenceRefs = appendMissingStrings(verification.EvidenceRefs, EvidenceRefsFromArtifacts(verification.Evidence)...)
	if verification.Status == "" && len(verification.Reasons) == 0 && len(verification.EvidenceRefs) == 0 && len(verification.Evidence) == 0 {
		return nil
	}
	return verification
}

func syntheticTriggerSource(req SyntheticDispatchRequest, copyProvenance []map[string]any) map[string]any {
	source := map[string]any{}
	for key, value := range req.TriggerSource {
		source[key] = value
	}
	source["kind"] = "manual_synthetic_dispatch"
	source["reason"] = req.Reason
	source["start_at_phase"] = req.StartAtPhase
	context := map[string]any{}
	if req.ExecutionContext.SlotLeaseRef != "" {
		context["slot_lease_ref"] = req.ExecutionContext.SlotLeaseRef
	}
	if req.ExecutionContext.Namespace != "" {
		context["namespace"] = req.ExecutionContext.Namespace
	}
	if req.ExecutionContext.ValidationURL != "" {
		context["validation_url"] = req.ExecutionContext.ValidationURL
	}
	if len(context) > 0 {
		source["execution_context"] = context
	}
	if len(copyProvenance) > 0 {
		source["copied_phase_outputs"] = copyProvenance
	}
	return source
}

func phaseWithIndex(phases []PhaseSpec, name string) (*PhaseSpec, int) {
	for i := range phases {
		if phases[i].Name == name {
			return &phases[i], i
		}
	}
	return nil, -1
}
