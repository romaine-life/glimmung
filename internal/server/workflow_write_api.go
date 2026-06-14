package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
	"github.com/romaine-life/glimmung/internal/domain/budget"
	"github.com/romaine-life/glimmung/internal/domain/phaserefs"
	"github.com/romaine-life/glimmung/internal/domain/whenexpr"
	"github.com/romaine-life/glimmung/internal/runnermcp"
)

const (
	workflowKindRunnerK8sJob = "k8s_job"

	PhaseRunOnSuccess = "success"
	PhaseRunOnFailure = "failure"
	PhaseRunOnAlways  = "always"

	PhasePurposeWork             = "work"
	PhasePurposeVerification     = "verification"
	PhasePurposeEvidenceGate     = "evidence_gate"
	PhasePurposeTeardown         = "teardown"
	PhasePurposeReviewTouchpoint = "review_touchpoint"
	PhasePurposeReviewGate       = "review_gate"

	PhaseNamePrepare = "prepare"

	MaxVerificationDynamicBlockItemCount = 10

	VerificationCaseJobCount             = 10
	VerificationCaseJobPrefix            = "verify-case-"
	MaxVerificationCaseJobTimeoutSeconds = 10 * 60

	VerificationShapeSingleJob          = "single_job"
	VerificationShapeBoundedCaseJobs    = "bounded_case_jobs"
	VerificationShapeDynamicStepGroup   = "dynamic_step_group"
	DefaultVerificationBoundedCaseCount = VerificationCaseJobCount

	// MinRunnerPhaseJobTimeoutSeconds is the floor for a phase job's
	// activeDeadlineSeconds. Below this the kubelet grace period
	// (30s default) doesn't leave enough room for the runner's SIGTERM
	// handler (glimmung#624) to deliver a /completed callback before
	// SIGKILL, so the run loses its terminal signal even when the
	// timeout trips for a benign reason. 60s = 30s grace + 30s margin
	// for the actual HTTP write + child reap.
	MinRunnerPhaseJobTimeoutSeconds = 60

	// MaxRunnerPhaseJobTimeoutSeconds is the ceiling. Six hours is
	// already well past the longest agent-run timeout we ship; values
	// above it are almost certainly a typo (e.g. milliseconds instead
	// of seconds inverted).
	MaxRunnerPhaseJobTimeoutSeconds = 6 * 60 * 60
)

// validPhaseKinds is the closed set of executor kinds Glimmung dispatches.
//
// k8s_job — the default executor. Phases of this kind launch one or more
// Kubernetes Jobs in parallel; phase completion is callback-driven.
var validPhaseKinds = map[string]bool{
	workflowKindRunnerK8sJob: true,
}

type WorkflowRegisterStore interface {
	UpsertWorkflow(ctx context.Context, req WorkflowRegister) (Workflow, error)
}

type WorkflowDeleteStore interface {
	DeleteWorkflow(ctx context.Context, project string, name string, actor string) (Workflow, error)
}

type WorkflowRegister struct {
	Project             string              `json:"project"`
	Name                string              `json:"name"`
	Phases              []PhaseSpec         `json:"phases"`
	PR                  PrPrimitive         `json:"pr"`
	Budget              budget.Config       `json:"budget"`
	Constraints         WorkflowConstraints `json:"constraints,omitempty"`
	DefaultRequirements map[string]any      `json:"default_requirements"`
	// Vars is the registration-owned variable map consumed by `when`
	// conditions. See Workflow.Vars for the contract.
	Vars map[string]string `json:"vars,omitempty"`
	// DispatchInputs declares the dispatch-time inputs the workflow needs.
	// See Workflow.DispatchInputs for the contract; ValidateWorkflowRegister
	// rejects any `${{ inputs.X }}` ref in checkout/extra_checkouts/workflow_ref
	// that does not name a declared input.
	DispatchInputs []DispatchInputSpec `json:"dispatch_inputs,omitempty"`
	Metadata       map[string]any      `json:"metadata"`
	// Actor is the server-derived attribution string for this write. It is
	// never decoded from the request body — the handler composes it from the
	// authenticated admin identity plus the optional X-Glimmung-Actor header.
	Actor string `json:"-"`
}

type WorkflowPatchStore interface {
	PatchWorkflow(ctx context.Context, project string, name string, req WorkflowPatchRequest) (Workflow, error)
}

// WorkflowPatchRequest carries the live rollout knobs that can change
// without re-registering the workflow's structural shape by hand. The
// budget total and the recycle-policy attempt counts are patchable; the
// historical PR opt-out toggle was deleted per migration-policy.
//
// Recycle counts are not structural shape: `on` and `lands_at` describe
// where a recycle lane lands and what fires it (the topology), while
// `max_attempts` is the guard-rail dial on that lane. Scaling the dial
// is the common operator need ("give this verify loop one more try"),
// so it gets a first-class patch surface that still flows through
// UpsertWorkflow — every change mints a new immutable schema and moves
// the logical pointer forward, exactly like a full re-registration.
type WorkflowPatchRequest struct {
	BudgetTotal *float64 `json:"budget_total"`
	// RecycleMaxAttempts scales the attempt count on existing recycle
	// lanes. Each entry targets a phase by name, or the workflow-level
	// PR reject lane via the sentinel target "pr". Targets without an
	// existing recycle policy are rejected: a count cannot conjure the
	// structural `on`/`lands_at` a lane needs.
	RecycleMaxAttempts []RecycleMaxAttemptsPatch `json:"recycle_max_attempts,omitempty"`
	// Actor is the server-derived attribution string for this write; see
	// WorkflowRegister.Actor.
	Actor string `json:"-"`
}

// WorkflowControlPinRequest pins the CURRENT value at a control target.
// Pinning does not set values — the existing patch surface does that — it
// freezes what is live, so a pin is always an explicit operator act against
// a value they can see.
type WorkflowControlPinRequest struct {
	Reason string `json:"reason,omitempty"`
	Actor  string `json:"-"`
}

type WorkflowControlPinStore interface {
	PinWorkflowControl(ctx context.Context, project, name, target string, req WorkflowControlPinRequest) (Workflow, error)
	UnpinWorkflowControl(ctx context.Context, project, name, target, actor string) (Workflow, error)
	ListWorkflowControlEvents(ctx context.Context, project, name string, limit int) ([]WorkflowControlEvent, error)
}

// Control pin target grammar. Pinnable controls are the guard-rail dials:
// recycle lanes and the budget. The vocabulary is closed on purpose — a pin
// on arbitrary payload paths would turn the pin document into a second
// schema language.
const (
	ControlPinTargetBudget = "budget"
	ControlPinTargetPR     = "pr.recycle_policy"
)

// ControlPinPhasePrefix + "<phase>" + ControlPinPhaseSuffix addresses a
// phase's recycle policy, e.g. "phases.llm-verify.recycle_policy".
const (
	ControlPinPhasePrefix = "phases."
	ControlPinPhaseSuffix = ".recycle_policy"
)

// ParseControlPinTarget validates a pin target and returns the phase name
// when the target addresses a phase recycle policy (empty otherwise).
func ParseControlPinTarget(target string) (phase string, err error) {
	switch {
	case target == ControlPinTargetBudget, target == ControlPinTargetPR:
		return "", nil
	case strings.HasPrefix(target, ControlPinPhasePrefix) && strings.HasSuffix(target, ControlPinPhaseSuffix):
		phase = strings.TrimSuffix(strings.TrimPrefix(target, ControlPinPhasePrefix), ControlPinPhaseSuffix)
		if strings.TrimSpace(phase) == "" {
			return "", ValidationError{Message: fmt.Sprintf("control pin target %q names no phase", target)}
		}
		return phase, nil
	default:
		return "", ValidationError{Message: fmt.Sprintf(
			"control pin target %q is not pinnable; targets are %q, %q, or %q<phase>%q",
			target, ControlPinTargetBudget, ControlPinTargetPR, ControlPinPhasePrefix, ControlPinPhaseSuffix,
		)}
	}
}

// ControlPinTargetForRecyclePatch maps a recycle_max_attempts patch target
// (phase name or the "pr" sentinel) to its control pin target.
func ControlPinTargetForRecyclePatch(target string) string {
	if target == RecyclePatchTargetPR {
		return ControlPinTargetPR
	}
	return ControlPinPhasePrefix + target + ControlPinPhaseSuffix
}

// RecyclePatchTargetPR is the sentinel RecycleMaxAttemptsPatch.Target
// that addresses the workflow-level PR reject recycle lane
// (pr.recycle_policy) rather than a named phase.
const RecyclePatchTargetPR = "pr"

// MinRecycleMaxAttempts is the floor for a recycle lane's attempt count.
// A lane must permit at least one attempt; zero would mean "never run
// this phase," which is expressed by removing the lane, not by scaling
// it to zero.
const MinRecycleMaxAttempts = 1

// MaxRecycleMaxAttempts is the ceiling. The verify loop is a token-spend
// surface (every retry costs money), and the cumulative cost budget is
// the primary ceiling; this attempt cap is a coarse second guard rail.
// Values past this are almost certainly a typo and would let a single
// run grind far past any reasonable retry budget before the cost ceiling
// catches it.
const MaxRecycleMaxAttempts = 20

// RecycleMaxAttemptsPatch scales the attempt count on one recycle lane.
type RecycleMaxAttemptsPatch struct {
	Target      string `json:"target"`
	MaxAttempts int    `json:"max_attempts"`
}

// ApplyRecycleMaxAttemptsPatches mutates reg in place, scaling the
// max_attempts on the addressed recycle lanes. It validates each patch
// and returns a ValidationError (HTTP 400) for bad input: an unknown
// target, a target whose lane does not exist, an out-of-range count, or
// a duplicate target in the same request.
func ApplyRecycleMaxAttemptsPatches(reg *WorkflowRegister, patches []RecycleMaxAttemptsPatch) error {
	if len(patches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, patch := range patches {
		target := strings.TrimSpace(patch.Target)
		if target == "" {
			return ValidationError{Message: "recycle_max_attempts entry is missing target"}
		}
		if _, dup := seen[target]; dup {
			return ValidationError{Message: fmt.Sprintf("recycle_max_attempts target %q is repeated", target)}
		}
		seen[target] = struct{}{}
		if patch.MaxAttempts < MinRecycleMaxAttempts || patch.MaxAttempts > MaxRecycleMaxAttempts {
			return ValidationError{Message: fmt.Sprintf(
				"recycle_max_attempts target %q max_attempts=%d is out of range [%d, %d]",
				target, patch.MaxAttempts, MinRecycleMaxAttempts, MaxRecycleMaxAttempts,
			)}
		}
		policy, err := recycleLaneForTarget(reg, target)
		if err != nil {
			return err
		}
		policy.MaxAttempts = patch.MaxAttempts
	}
	return nil
}

// recycleLaneForTarget returns the recycle policy a patch addresses, or a
// ValidationError when the target is unknown or carries no recycle lane.
func recycleLaneForTarget(reg *WorkflowRegister, target string) (*RecyclePolicy, error) {
	if target == RecyclePatchTargetPR {
		if reg.PR.RecyclePolicy == nil {
			return nil, ValidationError{Message: "recycle_max_attempts target \"pr\" has no recycle policy to scale"}
		}
		return reg.PR.RecyclePolicy, nil
	}
	for i := range reg.Phases {
		if reg.Phases[i].Name == target {
			if reg.Phases[i].RecyclePolicy == nil {
				return nil, ValidationError{Message: fmt.Sprintf("recycle_max_attempts target phase %q has no recycle policy to scale", target)}
			}
			return reg.Phases[i].RecyclePolicy, nil
		}
	}
	return nil, ValidationError{Message: fmt.Sprintf("recycle_max_attempts target phase %q does not exist", target)}
}

// ActorHeader is the optional caller-supplied attribution detail forwarded by
// trusted service callers (the MCP server forwards its own caller's identity,
// e.g. "tank-session:815"). It refines, never replaces, the authenticated
// identity: the recorded actor is always anchored to the verified principal.
const ActorHeader = "X-Glimmung-Actor"

// requestActor composes the attribution string for a control-plane write from
// the authenticated admin identity plus the optional forwarded actor detail.
func requestActor(r *http.Request) string {
	principal := ""
	if user, ok := adminUser(r.Context()); ok {
		principal = strings.TrimSpace(user.Sub)
		if principal == "" {
			principal = strings.TrimSpace(user.Email)
		}
		if actorEmail := strings.TrimSpace(user.ActorEmail); actorEmail != "" && actorEmail != principal {
			principal += " for " + actorEmail
		}
	}
	if forwarded := strings.TrimSpace(r.Header.Get(ActorHeader)); forwarded != "" {
		if principal == "" {
			return "(unverified) " + forwarded
		}
		return principal + " via " + forwarded
	}
	return principal
}

func registerWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(WorkflowRegisterStore)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow writer not configured")
			return
		}
		var req WorkflowRegister
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Actor = requestActor(r)
		project, ok, err := lookupProject(r.Context(), store, req.Project)
		if err != nil {
			writeInternalError(w, r, err, "read project failed")
			return
		}
		if !ok {
			writeProblem(w, http.StatusBadRequest, "project "+req.Project+" does not exist; register it first")
			return
		}
		normalizeWorkflowRegisterForProject(&req, project)
		if err := ValidateWorkflowRegister(req); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		workflow, err := writer.UpsertWorkflow(r.Context(), req)
		if validationErr, ok := err.(ValidationError); ok {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "register workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func patchWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		patcher, ok := store.(WorkflowPatchStore)
		if !ok || patcher == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow patcher not configured")
			return
		}
		var req WorkflowPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		req.Actor = requestActor(r)

		project := r.PathValue("project")
		name := r.PathValue("name")
		workflow, err := patcher.PatchWorkflow(r.Context(), project, name, req)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if validationErr, ok := err.(ValidationError); ok {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "patch workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func deleteWorkflow(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer, ok := store.(WorkflowDeleteStore)
		if !ok || writer == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow writer not configured")
			return
		}
		project := r.PathValue("project")
		name := r.PathValue("name")
		workflow, err := writer.DeleteWorkflow(r.Context(), project, name, requestActor(r))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "delete workflow failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func pinWorkflowControl(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pinner, ok := store.(WorkflowControlPinStore)
		if !ok || pinner == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow control pins not configured")
			return
		}
		var req WorkflowControlPinRequest
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeProblem(w, http.StatusBadRequest, "invalid JSON body")
				return
			}
		}
		req.Actor = requestActor(r)
		project := r.PathValue("project")
		name := r.PathValue("name")
		target := r.PathValue("target")
		if _, err := ParseControlPinTarget(target); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		workflow, err := pinner.PinWorkflowControl(r.Context(), project, name, target, req)
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if validationErr, ok := err.(ValidationError); ok {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "pin workflow control failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func unpinWorkflowControl(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pinner, ok := store.(WorkflowControlPinStore)
		if !ok || pinner == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow control pins not configured")
			return
		}
		project := r.PathValue("project")
		name := r.PathValue("name")
		target := r.PathValue("target")
		if _, err := ParseControlPinTarget(target); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error())
			return
		}
		workflow, err := pinner.UnpinWorkflowControl(r.Context(), project, name, target, requestActor(r))
		if errors.Is(err, ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "workflow "+project+"."+name+" not found")
			return
		}
		if validationErr, ok := err.(ValidationError); ok {
			writeProblem(w, http.StatusBadRequest, validationErr.Message)
			return
		}
		if err != nil {
			writeInternalError(w, r, err, "unpin workflow control failed")
			return
		}
		writeJSON(w, http.StatusOK, workflow)
	}
}

func listWorkflowControlEvents(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lister, ok := store.(WorkflowControlPinStore)
		if !ok || lister == nil {
			writeProblem(w, http.StatusServiceUnavailable, "workflow control events not configured")
			return
		}
		project := r.PathValue("project")
		name := r.PathValue("name")
		limit := 50
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 500 {
				writeProblem(w, http.StatusBadRequest, "limit must be an integer in [1, 500]")
				return
			}
			limit = parsed
		}
		events, err := lister.ListWorkflowControlEvents(r.Context(), project, name, limit)
		if err != nil {
			writeInternalError(w, r, err, "list workflow control events failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": events})
	}
}

func normalizeWorkflowRegister(req *WorkflowRegister) {
	normalizeWorkflowRegisterWithDefaultKind(req, workflowKindRunnerK8sJob)
}

func normalizeWorkflowRegisterForProject(req *WorkflowRegister, project Project) {
	normalizeWorkflowRegisterWithDefaultKind(req, workflowKindRunnerK8sJob)
}

func normalizeWorkflowRegisterWithDefaultKind(req *WorkflowRegister, defaultKind string) {
	if strings.TrimSpace(defaultKind) == "" {
		defaultKind = workflowKindRunnerK8sJob
	}
	if req.Budget.Total == 0 {
		req.Budget = budget.DefaultConfig()
	}
	req.DefaultRequirements = mapOrEmpty(req.DefaultRequirements)
	req.Metadata = mapOrEmpty(req.Metadata)
	for i := range req.DispatchInputs {
		req.DispatchInputs[i].Name = strings.TrimSpace(req.DispatchInputs[i].Name)
		req.DispatchInputs[i].Description = strings.TrimSpace(req.DispatchInputs[i].Description)
		req.DispatchInputs[i].Default = strings.TrimSpace(req.DispatchInputs[i].Default)
	}
	for i := range req.Phases {
		req.Phases[i].Kind = strings.TrimSpace(req.Phases[i].Kind)
		if req.Phases[i].Kind == "" {
			req.Phases[i].Kind = defaultKind
		}
		req.Phases[i].RunOn = strings.TrimSpace(req.Phases[i].RunOn)
		req.Phases[i].Purpose = strings.TrimSpace(req.Phases[i].Purpose)
		if req.Phases[i].WorkflowRef == "" {
			if runnerPhaseHasPrimaryCheckout(req.Phases[i]) {
				req.Phases[i].WorkflowRef = CanonicalGitRefTemplate
			} else {
				req.Phases[i].WorkflowRef = CanonicalGitRefDefault
			}
		}
		if req.Phases[i].Inputs == nil {
			req.Phases[i].Inputs = map[string]string{}
		}
		req.Phases[i].Outputs = sliceOrEmpty(req.Phases[i].Outputs)
		req.Phases[i].DependsOn = sliceOrEmpty(req.Phases[i].DependsOn)
		req.Phases[i].Jobs = sliceOrEmpty(req.Phases[i].Jobs)
		for j := range req.Phases[i].Jobs {
			job := &req.Phases[i].Jobs[j]
			job.Primitive = strings.TrimSpace(job.Primitive)
			job.Command = sliceOrEmpty(job.Command)
			job.Args = sliceOrEmpty(job.Args)
			if job.Env == nil {
				job.Env = map[string]string{}
			}
			job.Steps = sliceOrEmpty(job.Steps)
			job.ExtraCheckouts = sliceOrEmpty(job.ExtraCheckouts)
			for k := range job.Steps {
				step := &job.Steps[k]
				step.Type = strings.TrimSpace(step.Type)
				if step.Type == "" && strings.TrimSpace(step.Run) != "" {
					step.Type = "run"
				}
				if step.Env == nil {
					step.Env = map[string]string{}
				}
			}
		}
		req.Phases[i] = CanonicalRunnerPhase(req.Phases[i])
		if req.Phases[i].Purpose == "" {
			req.Phases[i].Purpose = phasePurpose(req.Phases[i])
		}
		if req.Phases[i].RunOn == "" {
			req.Phases[i].RunOn = phaseRunOn(req.Phases[i])
		}
	}
}

func lookupProject(ctx context.Context, store ReadStore, name string) (Project, bool, error) {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return Project{}, false, err
	}
	for _, project := range projects {
		if firstNonEmpty(project.Name, project.ID) == name {
			return project, true, nil
		}
	}
	return Project{}, false, nil
}

func validateWorkflowAllowedForProject(project Project, req WorkflowRegister) error {
	for _, phase := range req.Phases {
		if err := validateRunnerWorkflowKind(phase.Kind); err != nil {
			return err
		}
	}
	return nil
}

func CanonicalWorkflowConstraints(req WorkflowRegister) WorkflowConstraints {
	constraints := req.Constraints
	constraints.Verification.Shape = strings.TrimSpace(constraints.Verification.Shape)
	if constraints.Verification.Shape == "" {
		constraints.Verification.Shape = inferVerificationConstraintShape(req.Phases)
	}
	if constraints.Verification.Shape == VerificationShapeBoundedCaseJobs && constraints.Verification.MaxCases == 0 {
		constraints.Verification.MaxCases = DefaultVerificationBoundedCaseCount
	}
	return constraints
}

func inferVerificationConstraintShape(phases []PhaseSpec) string {
	for _, phase := range phases {
		if !phase.Verify {
			continue
		}
		if len(phase.Jobs) == 1 {
			for _, step := range phase.Jobs[0].Steps {
				if step.DynamicGroup != nil {
					return VerificationShapeDynamicStepGroup
				}
			}
			return VerificationShapeSingleJob
		}
		return VerificationShapeBoundedCaseJobs
	}
	return VerificationShapeSingleJob
}

// ValidateWorkflowRegister enforces the persisted workflow graph contract.
func ValidateWorkflowRegister(req WorkflowRegister) error {
	if len(req.Phases) == 0 {
		return ValidationError{Message: "workflow " + req.Name + " is missing required phases: prepare, testing, cleanup"}
	}
	if err := validateWorkflowAllowedForProject(Project{}, req); err != nil {
		return err
	}
	declaredInputs, err := validateDispatchInputs(req.Name, req.DispatchInputs)
	if err != nil {
		return err
	}
	if err := validateDispatchInputRefs(req.Name, req.Phases, declaredInputs); err != nil {
		return err
	}
	if err := validateWorkflowVars(req.Name, req.Vars); err != nil {
		return err
	}
	whenInputs := make(map[string]bool, len(declaredInputs))
	for key := range declaredInputs {
		whenInputs[key] = true
	}
	constraints := CanonicalWorkflowConstraints(req)
	if err := validateWorkflowConstraints(req.Name, constraints); err != nil {
		return err
	}
	phaseRefs := make([]phaserefs.Phase, 0, len(req.Phases))
	phaseNames := map[string]int{}
	testingCount := 0
	hasCleanup := false
	prTouchpointJobs := 0
	reviewGateCount := 0
	for i, phase := range req.Phases {
		name := strings.TrimSpace(phase.Name)
		if name == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase[%d] is missing name", req.Name, i)}
		}
		if prev, ok := phaseNames[name]; ok {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q duplicates phase[%d]", req.Name, name, prev)}
		}
		phaseNames[name] = i
		explicitRunOn := strings.TrimSpace(phase.RunOn)
		explicitPurpose := strings.TrimSpace(phase.Purpose)
		if explicitRunOn != "" && !validPhaseRunOn(explicitRunOn) {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q run_on=%q is not one of [success, failure, always]", req.Name, name, explicitRunOn)}
		}
		if explicitPurpose != "" && !validPhasePurpose(explicitPurpose) {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q purpose=%q is not one of [work, verification, evidence_gate, teardown, review_touchpoint, review_gate]", req.Name, name, explicitPurpose)}
		}
		if phase.Always {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q uses retired field always; use run_on and purpose instead", req.Name, name)}
		}
		runOn := phaseRunOn(phase)
		purpose := phasePurpose(phase)
		if phase.EvidenceVerificationGate || purpose == PhasePurposeEvidenceGate {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q uses the retired evidence_verification_gate shape; verification phases must own evidence verdicts directly", req.Name, name)}
		}
		if phase.Verify {
			if purpose != PhasePurposeVerification {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q has verify=true and must set purpose=%q", req.Name, name, PhasePurposeVerification)}
			}
			if phase.RecyclePolicy == nil {
				// Silence is not a policy. The verify loop's retry budget is
				// an operator-owned control; a verification phase that omits
				// it would historically inherit whatever norm the registering
				// agent assumed. Require the choice to be written down:
				// max_attempts=1 runs the gate with recycling off.
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q is a verification phase and must declare recycle_policy explicitly (max_attempts=1 disables recycling); implicit defaults are prohibited", req.Name, name)}
			}
			if err := validateVerificationJob(req.Name, phase, constraints.Verification); err != nil {
				return err
			}
			testingCount++
		} else if purpose == PhasePurposeVerification {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q purpose=%q must set verify=true", req.Name, name, PhasePurposeVerification)}
		}
		if purpose == PhasePurposeTeardown {
			if runOn == PhaseRunOnSuccess {
				return ValidationError{Message: fmt.Sprintf("workflow %s teardown phase %q cannot set run_on=%q; teardown must run on failure or always", req.Name, name, PhaseRunOnSuccess)}
			}
			hasCleanup = true
			if len(phase.Inputs) > 0 {
				return ValidationError{Message: fmt.Sprintf("workflow %s teardown phase %q cannot declare inputs; cleanup must be abort-safe", req.Name, name)}
			}
		} else if runOn != PhaseRunOnSuccess {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q purpose=%q cannot set run_on=%q; only teardown phases may run on failure paths", req.Name, name, purpose, runOn)}
		}
		if phase.SkipWhenPreserveTestEnv {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q uses retired field skip_when_preserve_test_env; declare when: \"${{ run.preserve_test_env }} == 'false'\" on the phase instead", req.Name, name)}
		}
		// Conditions skip declared legs; gates are not legs. A verification
		// or review-gate phase that could evaluate itself away would bypass
		// the gate it exists to be — gates run.
		if strings.TrimSpace(phase.When) != "" {
			if phase.Verify || purpose == PhasePurposeVerification || purpose == PhasePurposeReviewGate {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q (purpose=%q) cannot declare when; verification and review-gate phases always run", req.Name, name, purpose)}
			}
			// Entry phases (no depends_on) are dispatch targets for fresh
			// cycles and recycle landings; a conditional entry would leave
			// the run with no defined start.
			if len(phase.DependsOn) == 0 {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q is an entry phase (no depends_on) and cannot declare when; entry phases always run", req.Name, name)}
			}
			if err := whenexpr.Validate(phase.When, fmt.Sprintf("workflow %s phase %q", req.Name, name), req.Vars, whenInputs); err != nil {
				return ValidationError{Message: err.Error()}
			}
		}
		conditionalJobs := 0
		for _, job := range phase.Jobs {
			if strings.TrimSpace(job.When) == "" {
				continue
			}
			conditionalJobs++
			if phase.Verify || purpose == PhasePurposeVerification || purpose == PhasePurposeReviewGate {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q (purpose=%q) cannot declare when; verification and review-gate jobs always run", req.Name, name, job.ID, purpose)}
			}
			if err := whenexpr.Validate(job.When, fmt.Sprintf("workflow %s phase %q job %q", req.Name, name, job.ID), req.Vars, whenInputs); err != nil {
				return ValidationError{Message: err.Error()}
			}
		}
		// At least one job per phase stays unconditional, so a dispatched
		// phase always launches something and the completion callback path
		// always has a real job to flip CompletionReady. Whole-phase
		// conditionality is the phase-level when's job.
		if len(phase.Jobs) > 0 && conditionalJobs == len(phase.Jobs) {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q declares when on every job; at least one job must be unconditional (use a phase-level when for whole-phase conditions)", req.Name, name)}
		}
		if purpose == PhasePurposeReviewGate {
			reviewGateCount++
			if phase.Verify {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q has purpose=%q and cannot also be the verify phase", req.Name, name, PhasePurposeReviewGate)}
			}
			if runOn != PhaseRunOnSuccess {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q has purpose=%q and must set run_on=%q", req.Name, name, PhasePurposeReviewGate, PhaseRunOnSuccess)}
			}
			if phase.EvidenceVerificationGate {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q has purpose=%q and cannot also be an evidence_verification_gate", req.Name, name, PhasePurposeReviewGate)}
			}
			if phase.RecyclePolicy != nil {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q has purpose=%q and cannot declare a recycle_policy; reject is handled by the workflow-level pr.recycle_policy", req.Name, name, PhasePurposeReviewGate)}
			}
			if len(phase.Jobs) != 1 || strings.TrimSpace(phase.Jobs[0].Primitive) != JobPrimitivePRMerge {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q has purpose=%q and must declare exactly one job with primitive %q", req.Name, name, PhasePurposeReviewGate, JobPrimitivePRMerge)}
			}
		}
		for _, job := range phase.Jobs {
			if job.Checkout != nil && strings.TrimSpace(job.Checkout.Repo) != "" {
				return ValidationError{Message: fmt.Sprintf("workflow %s job %q in phase %q sets checkout.repo %q; the primary checkout repo is derived from the project's github_repo and must be empty (use extra_checkouts for additional repos)", req.Name, job.ID, name, job.Checkout.Repo)}
			}
		}
		if len(phase.Jobs) > 0 {
			seenJobs := map[string]int{}
			for j, job := range phase.Jobs {
				jobID := strings.TrimSpace(job.ID)
				if jobID == "" {
					return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job[%d] is missing id", req.Name, name, j)}
				}
				if prev, ok := seenJobs[jobID]; ok {
					return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q duplicates job[%d]", req.Name, name, jobID, prev)}
				}
				seenJobs[jobID] = j
				switch strings.TrimSpace(job.Primitive) {
				case "":
				case JobPrimitivePRTouchpoint:
					prTouchpointJobs++
					if purpose != PhasePurposeReviewTouchpoint {
						return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q primitive %q must be in a purpose=%q phase", req.Name, name, job.ID, JobPrimitivePRTouchpoint, PhasePurposeReviewTouchpoint)}
					}
					if runOn != PhaseRunOnSuccess {
						return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q primitive %q must run only on successful verification paths", req.Name, name, job.ID, JobPrimitivePRTouchpoint)}
					}
				case JobPrimitivePRMerge:
					if purpose != PhasePurposeReviewGate {
						return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q primitive %q must live inside a purpose=%q phase", req.Name, name, job.ID, JobPrimitivePRMerge, PhasePurposeReviewGate)}
					}
				default:
					return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q declares unknown primitive %q", req.Name, name, job.ID, job.Primitive)}
				}
				if err := validateRunnerJobSpec(req.Name, name, j, job); err != nil {
					return err
				}
			}
		}
		if purpose == PhasePurposeReviewTouchpoint && !phaseHasPrimitive(phase, JobPrimitivePRTouchpoint) {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q purpose=%q must declare exactly one job with primitive %q", req.Name, name, PhasePurposeReviewTouchpoint, JobPrimitivePRTouchpoint)}
		}
		if i == 0 {
			if name != PhaseNamePrepare {
				return ValidationError{Message: fmt.Sprintf("workflow %s entry phase must be named %q", req.Name, PhaseNamePrepare)}
			}
			if len(phase.DependsOn) != 0 {
				return ValidationError{Message: fmt.Sprintf("workflow %s entry phase %q must not declare depends_on", req.Name, name)}
			}
		} else {
			if len(phase.DependsOn) != 1 {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q must declare exactly one depends_on entry", req.Name, name)}
			}
			want := req.Phases[i-1].Name
			if phase.DependsOn[0] != want {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q depends_on must be [%q]", req.Name, name, want)}
			}
		}
		phaseRefs = append(phaseRefs, phaserefs.Phase{
			Name:    name,
			Inputs:  phase.Inputs,
			Outputs: phase.Outputs,
		})
	}
	missing := make([]string, 0, 4)
	if testingCount == 0 {
		missing = append(missing, "verify")
	}
	if !hasCleanup {
		missing = append(missing, "teardown cleanup")
	}
	if reviewGateCount == 0 {
		missing = append(missing, "review_gate touchpoint_gate")
	}
	if prTouchpointJobs == 0 {
		missing = append(missing, "pr_touchpoint primitive")
	}
	if len(missing) > 0 {
		return ValidationError{Message: "workflow " + req.Name + " is missing required phases: " + strings.Join(missing, ", ")}
	}
	if testingCount > 1 {
		return ValidationError{Message: fmt.Sprintf("workflow %s declares %d verify=true phases; exactly one bounded verification phase is required", req.Name, testingCount)}
	}
	if reviewGateCount > 1 {
		return ValidationError{Message: fmt.Sprintf("workflow %s declares %d purpose=%q phases; exactly one is required", req.Name, reviewGateCount, PhasePurposeReviewGate)}
	}
	if prTouchpointJobs > 1 {
		return ValidationError{Message: fmt.Sprintf("workflow %s declares %d %q primitives; exactly one is required", req.Name, prTouchpointJobs, JobPrimitivePRTouchpoint)}
	}
	if err := phaserefs.Validate(phaseRefs); err != nil {
		return ValidationError{Message: err.Error()}
	}
	if err := validateMandatoryGitRefFeature(req.Name, req.Phases, declaredInputs); err != nil {
		return err
	}
	return nil
}

// validateDispatchInputs returns the declared input names as a lookup set.
// Each entry's Name must follow the run-input identifier rule, must be unique,
// and a non-required input must have a Default value (a non-required missing
// value is a malformed spec — there is nothing to substitute and no contract
// to enforce). Default is rendered as the substituted value as-is.
// validateWorkflowVars enforces the vars-key grammar so `${{ vars.<key> }}`
// refs in when conditions are unambiguous. Values are free-form strings.
func validateWorkflowVars(workflowName string, vars map[string]string) error {
	for key := range vars {
		name := strings.TrimSpace(key)
		if name == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s vars declares an empty key", workflowName)}
		}
		if !runInputNamePattern.MatchString(name) {
			return ValidationError{Message: fmt.Sprintf("workflow %s vars key %q is invalid; use letters, numbers, underscores, or hyphens, starting with a letter or underscore", workflowName, key)}
		}
	}
	return nil
}

func validateDispatchInputs(workflowName string, inputs []DispatchInputSpec) (map[string]DispatchInputSpec, error) {
	declared := map[string]DispatchInputSpec{}
	for i, input := range inputs {
		name := strings.TrimSpace(input.Name)
		if name == "" {
			return nil, ValidationError{Message: fmt.Sprintf("workflow %s dispatch_inputs[%d] is missing name", workflowName, i)}
		}
		if !runInputNamePattern.MatchString(name) {
			return nil, ValidationError{Message: fmt.Sprintf("workflow %s dispatch_inputs[%d] name %q is invalid; use letters, numbers, underscores, or hyphens, starting with a letter or underscore", workflowName, i, input.Name)}
		}
		if _, dup := declared[name]; dup {
			return nil, ValidationError{Message: fmt.Sprintf("workflow %s dispatch_inputs[%d] name %q duplicates an earlier entry", workflowName, i, name)}
		}
		if !input.Required && strings.TrimSpace(input.Default) == "" {
			return nil, ValidationError{Message: fmt.Sprintf("workflow %s dispatch_inputs[%d] name %q is not required but has no default; non-required inputs must declare a default to substitute", workflowName, i, name)}
		}
		declared[name] = input
	}
	return declared, nil
}

// validateDispatchInputRefs walks every templated checkout.ref,
// extra_checkouts[].ref, and phase.workflow_ref and requires each
// `${{ inputs.X }}` reference to name a declared dispatch input. Mirrors the
// cross-phase-ref check (phaserefs.Validate) but for dispatch-time inputs —
// a workflow that promises to substitute X must declare X or be rejected at
// register time. Empty refs and literal (non-templated) refs are passed.
func validateDispatchInputRefs(workflowName string, phases []PhaseSpec, declared map[string]DispatchInputSpec) error {
	check := func(location, value string) error {
		matches := runInputRefPattern.FindStringSubmatch(value)
		if matches == nil {
			return nil
		}
		key := matches[1]
		if _, ok := declared[key]; !ok {
			return ValidationError{Message: fmt.Sprintf("workflow %s %s refs ${{ inputs.%s }} but dispatch_inputs does not declare %q; add it to dispatch_inputs or change the ref to a literal", workflowName, location, key, key)}
		}
		return nil
	}
	for _, phase := range phases {
		phaseLocation := fmt.Sprintf("phase %q", phase.Name)
		if err := check(phaseLocation+" workflow_ref", phase.WorkflowRef); err != nil {
			return err
		}
		for _, job := range phase.Jobs {
			jobLocation := fmt.Sprintf("phase %q job %q", phase.Name, job.ID)
			if job.Checkout != nil {
				if err := check(jobLocation+" checkout.ref", job.Checkout.Ref); err != nil {
					return err
				}
			}
			for j, extra := range job.ExtraCheckouts {
				if err := check(fmt.Sprintf("%s extra_checkouts[%d].ref", jobLocation, j), extra.Ref); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateMandatoryGitRefFeature(workflowName string, phases []PhaseSpec, declared map[string]DispatchInputSpec) error {
	requiresGitRef := false
	for _, phase := range phases {
		for _, job := range phase.Jobs {
			if job.Checkout == nil {
				continue
			}
			requiresGitRef = true
		}
	}
	if !requiresGitRef {
		return nil
	}
	input, ok := declared[CanonicalGitRefInput]
	if !ok {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s uses project checkouts and must declare dispatch_inputs entry %q",
			workflowName,
			CanonicalGitRefInput,
		)}
	}
	if !input.Required {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s dispatch_inputs.%s must set required=true; project checkout refs are mandatory run inputs",
			workflowName,
			CanonicalGitRefInput,
		)}
	}
	if strings.TrimSpace(input.Default) == "" {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s dispatch_inputs.%s must declare a non-empty default such as %q",
			workflowName,
			CanonicalGitRefInput,
			CanonicalGitRefDefault,
		)}
	}
	for _, phase := range phases {
		phaseHasPrimaryCheckout := false
		for _, job := range phase.Jobs {
			if job.Checkout == nil {
				continue
			}
			phaseHasPrimaryCheckout = true
			ref := strings.TrimSpace(job.Checkout.Ref)
			if ref != CanonicalGitRefTemplate {
				return ValidationError{Message: fmt.Sprintf(
					"workflow %s phase %q job %q checkout.ref must be %q so project checkouts are dispatchable via git_ref; got %q",
					workflowName,
					phase.Name,
					job.ID,
					CanonicalGitRefTemplate,
					job.Checkout.Ref,
				)}
			}
		}
		if phaseHasPrimaryCheckout {
			ref := strings.TrimSpace(phase.WorkflowRef)
			if ref != "" && ref != CanonicalGitRefTemplate {
				return ValidationError{Message: fmt.Sprintf(
					"workflow %s phase %q workflow_ref must be %q when the phase has a project checkout; got %q",
					workflowName,
					phase.Name,
					CanonicalGitRefTemplate,
					phase.WorkflowRef,
				)}
			}
		}
	}
	return nil
}

func runnerPhaseHasPrimaryCheckout(phase PhaseSpec) bool {
	for _, job := range phase.Jobs {
		if job.Checkout != nil {
			return true
		}
	}
	return false
}

func validateWorkflowConstraints(workflowName string, constraints WorkflowConstraints) error {
	switch constraints.Verification.Shape {
	case VerificationShapeSingleJob, VerificationShapeBoundedCaseJobs, VerificationShapeDynamicStepGroup:
	default:
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s constraints.verification.shape=%q is not one of [%s, %s, %s]",
			workflowName,
			constraints.Verification.Shape,
			VerificationShapeSingleJob,
			VerificationShapeBoundedCaseJobs,
			VerificationShapeDynamicStepGroup,
		)}
	}
	if constraints.Verification.MaxCases < 0 || constraints.Verification.MaxCases > DefaultVerificationBoundedCaseCount {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s constraints.verification.max_cases=%d is out of range [0,%d]",
			workflowName,
			constraints.Verification.MaxCases,
			DefaultVerificationBoundedCaseCount,
		)}
	}
	if constraints.Verification.Shape == VerificationShapeBoundedCaseJobs && constraints.Verification.MaxCases == 0 {
		return ValidationError{Message: fmt.Sprintf("workflow %s constraints.verification.max_cases is required for shape=%q", workflowName, VerificationShapeBoundedCaseJobs)}
	}
	return nil
}

func validateVerificationJob(workflowName string, phase PhaseSpec, constraints VerificationConstraints) error {
	switch constraints.Shape {
	case VerificationShapeSingleJob:
		return validateSingleVerificationJob(workflowName, phase)
	case VerificationShapeBoundedCaseJobs:
		return validateBoundedVerificationCaseJobs(workflowName, phase, constraints.MaxCases)
	case VerificationShapeDynamicStepGroup:
		return validateDynamicVerificationJob(workflowName, phase)
	default:
		return ValidationError{Message: fmt.Sprintf("workflow %s constraints.verification.shape=%q is unsupported", workflowName, constraints.Shape)}
	}
}

func validateSingleVerificationJob(workflowName string, phase PhaseSpec) error {
	if len(phase.Jobs) != 1 {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s verification phase %q shape=%q must declare exactly one verification job",
			workflowName, phase.Name, VerificationShapeSingleJob,
		)}
	}
	if strings.TrimSpace(phase.Jobs[0].ID) == "" {
		return ValidationError{Message: fmt.Sprintf("workflow %s verification phase %q shape=%q job is missing id", workflowName, phase.Name, VerificationShapeSingleJob)}
	}
	return nil
}

func validateBoundedVerificationCaseJobs(workflowName string, phase PhaseSpec, maxCases int) error {
	if len(phase.Jobs) != maxCases {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s verification phase %q shape=%q must declare exactly %d bounded case jobs verify-case-01..verify-case-%02d",
			workflowName, phase.Name, VerificationShapeBoundedCaseJobs, maxCases, maxCases,
		)}
	}
	for i, job := range phase.Jobs {
		want := verificationCaseJobID(i + 1)
		if strings.TrimSpace(job.ID) != want {
			return ValidationError{Message: fmt.Sprintf(
				"workflow %s verification phase %q shape=%q job[%d] id=%q; want %q",
				workflowName, phase.Name, VerificationShapeBoundedCaseJobs, i, job.ID, want,
			)}
		}
		if job.TimeoutSeconds == nil {
			return ValidationError{Message: fmt.Sprintf(
				"workflow %s verification case job %q must set timeout_seconds; case execution must be bounded",
				workflowName, job.ID,
			)}
		}
		if *job.TimeoutSeconds > MaxVerificationCaseJobTimeoutSeconds {
			return ValidationError{Message: fmt.Sprintf(
				"workflow %s verification case job %q timeout_seconds=%d exceeds maximum %d; split or narrow the case instead of granting a monolithic verifier budget",
				workflowName, job.ID, *job.TimeoutSeconds, MaxVerificationCaseJobTimeoutSeconds,
			)}
		}
	}
	return nil
}

func validateDynamicVerificationJob(workflowName string, phase PhaseSpec) error {
	if len(phase.Jobs) != 1 {
		return ValidationError{Message: fmt.Sprintf(
			"workflow %s verification phase %q must declare exactly one sequential verification job with a dynamic test-case block",
			workflowName, phase.Name,
		)}
	}
	job := phase.Jobs[0]
	blockGroup := ""
	blockMaxItems := 0
	blockCount := 0
	blockStarted := false
	blockClosed := false
	for _, step := range job.Steps {
		if step.DynamicGroup == nil {
			if blockStarted {
				blockClosed = true
			}
			continue
		}
		if blockClosed {
			return ValidationError{Message: fmt.Sprintf("workflow %s verification job %q dynamic test-case group %q must be one contiguous step block", workflowName, job.ID, blockGroup)}
		}
		blockStarted = true
		blockCount++
		group := strings.TrimSpace(step.Group)
		if group == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s verification job %q step %q declares dynamic_group without group", workflowName, job.ID, step.Slug)}
		}
		maxItems := step.DynamicGroup.MaxItems
		if maxItems < 1 || maxItems > MaxVerificationDynamicBlockItemCount {
			return ValidationError{Message: fmt.Sprintf("workflow %s verification job %q step %q dynamic_group.max_items=%d is not in [1,%d]", workflowName, job.ID, step.Slug, maxItems, MaxVerificationDynamicBlockItemCount)}
		}
		if blockGroup == "" {
			blockGroup = group
			blockMaxItems = maxItems
			continue
		}
		if group != blockGroup {
			return ValidationError{Message: fmt.Sprintf("workflow %s verification job %q declares multiple dynamic test-case groups %q and %q", workflowName, job.ID, blockGroup, group)}
		}
		if maxItems != blockMaxItems {
			return ValidationError{Message: fmt.Sprintf("workflow %s verification job %q dynamic test-case group %q has inconsistent max_items", workflowName, job.ID, group)}
		}
	}
	if blockCount == 0 {
		return ValidationError{Message: fmt.Sprintf("workflow %s verification job %q must declare a dynamic test-case block", workflowName, job.ID)}
	}
	return nil
}

func verificationCaseJobID(index int) string {
	return fmt.Sprintf("%s%02d", VerificationCaseJobPrefix, index)
}

func validateRunnerJobSpec(workflowName, phaseName string, jobIndex int, job RunnerJobSpec) error {
	if job.Managed {
		if len(job.Command) > 0 || len(job.Args) > 0 {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q is managed and cannot declare command or args", workflowName, phaseName, job.ID)}
		}
	}
	// Runner tools (the per-job MCP allow-list) are only meaningful for managed
	// agent jobs: the launcher injects the agent's MCP config pointing at the
	// scoped sidecar, and only managed jobs run a Glimmung-launched agent. Every
	// declared name must be a known runner tool, rejected here at registration
	// rather than late at pod start when Scoped() would reject it.
	if len(job.Tools) > 0 {
		if !job.Managed {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q declares tools but is not managed; runner tools are only available to managed agent jobs", workflowName, phaseName, job.ID)}
		}
		seenTools := map[string]bool{}
		for _, raw := range job.Tools {
			tool := strings.TrimSpace(raw)
			if tool == "" {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q declares an empty tool name", workflowName, phaseName, job.ID)}
			}
			if seenTools[tool] {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q declares tool %q more than once", workflowName, phaseName, job.ID, tool)}
			}
			seenTools[tool] = true
			if !runnermcp.IsCatalogTool(tool) {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q declares unknown runner tool %q (known: %s)", workflowName, phaseName, job.ID, tool, strings.Join(runnermcp.CatalogToolNames(), ", "))}
			}
		}
	}
	// Timeout guardrail. activeDeadlineSeconds = job.TimeoutSeconds when
	// set; below MinRunnerPhaseJobTimeoutSeconds there is not enough
	// kubelet grace for the runner's SIGTERM handler (glimmung#624) to
	// deliver /completed before SIGKILL, so the run loses its terminal
	// signal even on a normal deadline trip. The ceiling catches typos.
	// Nil means "no deadline"; we still allow that — the dispatch-timeout
	// reconciler is the safety net.
	if job.TimeoutSeconds != nil {
		t := *job.TimeoutSeconds
		if t < MinRunnerPhaseJobTimeoutSeconds {
			return ValidationError{Message: fmt.Sprintf(
				"workflow %s phase %q job %q timeout_seconds=%d is below minimum %d; "+
					"the runner needs at least %d seconds of kubelet grace to deliver /completed before SIGKILL",
				workflowName, phaseName, job.ID, t, MinRunnerPhaseJobTimeoutSeconds, MinRunnerPhaseJobTimeoutSeconds,
			)}
		}
		if t > MaxRunnerPhaseJobTimeoutSeconds {
			return ValidationError{Message: fmt.Sprintf(
				"workflow %s phase %q job %q timeout_seconds=%d exceeds maximum %d (6h); likely a typo",
				workflowName, phaseName, job.ID, t, MaxRunnerPhaseJobTimeoutSeconds,
			)}
		}
	}
	seenSteps := map[string]int{}
	for i, step := range job.Steps {
		slug := strings.TrimSpace(step.Slug)
		if slug == "" {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step[%d] is missing slug", workflowName, phaseName, job.ID, i)}
		}
		if prev, ok := seenSteps[slug]; ok {
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q duplicates step[%d]", workflowName, phaseName, job.ID, slug, prev)}
		}
		seenSteps[slug] = i
		stepType := strings.TrimSpace(step.Type)
		if stepType == "" {
			stepType = "run"
		}
		if !job.Managed {
			if stepType == "agent" {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q uses type=agent but the job is not managed", workflowName, phaseName, job.ID, slug)}
			}
			continue
		}
		switch stepType {
		case "run":
			if strings.TrimSpace(step.Run) == "" {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q is missing run", workflowName, phaseName, job.ID, slug)}
			}
			if step.Agent != nil {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q cannot declare agent config on type=run", workflowName, phaseName, job.ID, slug)}
			}
		case "agent":
			if strings.TrimSpace(step.Run) != "" {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q uses type=agent and cannot declare run", workflowName, phaseName, job.ID, slug)}
			}
			if step.Agent == nil {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q uses type=agent and must declare agent config", workflowName, phaseName, job.ID, slug)}
			}
			if err := agentruntime.ValidateSlot(step.Agent.Slot); err != nil {
				return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q %s", workflowName, phaseName, job.ID, slug, err.Error())}
			}
		default:
			return ValidationError{Message: fmt.Sprintf("workflow %s phase %q job %q step %q uses unsupported type %q", workflowName, phaseName, job.ID, slug, stepType)}
		}
	}
	return nil
}

func workflowPhaseKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return workflowKindRunnerK8sJob
	}
	return kind
}

func validateRunnerWorkflowKind(kind string) error {
	if validPhaseKinds[workflowPhaseKind(kind)] {
		return nil
	}
	return ValidationError{Message: fmt.Sprintf("workflow phase kind %q is not one of [k8s_job]", workflowPhaseKind(kind))}
}

func validPhaseRunOn(value string) bool {
	switch value {
	case PhaseRunOnSuccess, PhaseRunOnFailure, PhaseRunOnAlways:
		return true
	default:
		return false
	}
}

func validPhasePurpose(value string) bool {
	switch value {
	case PhasePurposeWork,
		PhasePurposeVerification,
		PhasePurposeEvidenceGate,
		PhasePurposeTeardown,
		PhasePurposeReviewTouchpoint,
		PhasePurposeReviewGate:
		return true
	default:
		return false
	}
}

func NormalizePhaseRunOn(phase PhaseSpec) string {
	return phaseRunOn(phase)
}

func NormalizePhasePurpose(phase PhaseSpec) string {
	return phasePurpose(phase)
}

func phaseHasPrimitive(phase PhaseSpec, primitive string) bool {
	for _, job := range phase.Jobs {
		if strings.TrimSpace(job.Primitive) == primitive {
			return true
		}
	}
	return false
}

func projectRequiresRunnerWorkflows(project Project) bool {
	metadata := project.Metadata
	kind := strings.ToLower(firstNonEmpty(
		stringValue(metadata["app_kind"]),
		stringValue(metadata["appKind"]),
		stringValue(metadata["app_type"]),
		stringValue(metadata["appType"]),
		stringValue(metadata["kind"]),
	))
	return kind == "webapp"
}
