package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/romaine-life/glimmung/internal/domain/budget"
	"github.com/romaine-life/glimmung/internal/metrics"
)

type ReadStore interface {
	ListProjects(ctx context.Context) ([]Project, error)
	ListWorkflows(ctx context.Context) ([]Workflow, error)
}

type Project struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	GitHubRepo      string         `json:"github_repo"`
	ArgoCDApp       string         `json:"argocd_app"`
	ConfigSchemaRef string         `json:"config_schema_ref,omitempty"`
	Metadata        map[string]any `json:"metadata"`
	CreatedAt       time.Time      `json:"created_at"`
	// etag carries an optional store-provided CAS token. Empty for the
	// Postgres runtime path.
	etag string `json:"-"`
}

// ETag exposes the store-provided CAS token captured by the read that produced
// this Project. Empty when the current store path does not provide one.
func (p Project) ETag() string { return p.etag }

// WithETag returns a copy of the project with `tag` as its captured etag.
// Used by store implementations and tests to attach the etag at read time.
func (p Project) WithETag(tag string) Project { p.etag = tag; return p }

type Workflow struct {
	ID                  string              `json:"id"`
	Project             string              `json:"project"`
	Name                string              `json:"name"`
	SchemaRef           string              `json:"schema_ref"`
	Phases              []PhaseSpec         `json:"phases"`
	PR                  PrPrimitive         `json:"pr"`
	Budget              budget.Config       `json:"budget"`
	Constraints         WorkflowConstraints `json:"constraints,omitempty"`
	DefaultRequirements map[string]any      `json:"default_requirements"`
	// Vars is the registration-owned variable map referenced by phase- and
	// job-level `when` conditions (`${{ vars.<key> }}`). Vars are workflow
	// identity, not per-dispatch knobs: they name durable facts about the
	// shape (e.g. feature_type: effect) so conditions are reconstructable
	// from the schema snapshot alone. Part of the content-hashed schema.
	Vars map[string]string `json:"vars,omitempty"`
	// DispatchInputs declares the dispatch-time inputs the workflow needs.
	// Project checkouts must expose the canonical git_ref input and set
	// primary checkout refs to `${{ inputs.git_ref }}`. Every other
	// `${{ inputs.X }}` reference inside a phase's WorkflowRef or any job's
	// Checkout/ExtraCheckouts refs must name a declared input. The dispatcher
	// (dashboard, MCP, replay) reads this list to render forms and validate
	// payloads.
	DispatchInputs []DispatchInputSpec `json:"dispatch_inputs,omitempty"`
	Metadata       map[string]any      `json:"metadata"`
	CreatedAt      time.Time           `json:"created_at"`
	// ControlPins is the operator-owned pin document for this workflow.
	// Pinned control values survive any re-registration: the register path
	// enforces them into the authored payload and reports the override.
	// Populated from the workflows.control_pins column on row reads; never
	// part of the content-hashed schema payload.
	ControlPins map[string]ControlPin `json:"control_pins,omitempty"`
	// ControlChanges and PinsEnforced are write-outcome fields populated
	// only on register/patch/pin responses (omitted on plain reads): the
	// control-field diff vs. the previous registration, and the pin targets
	// whose incoming values were overridden by operator pins.
	ControlChanges []ControlChange `json:"control_changes,omitempty"`
	PinsEnforced   []string        `json:"pins_enforced,omitempty"`
}

// ControlPin freezes one control value on a workflow. Value is the canonical
// JSON of the pinned control (a recycle policy object, or {"total": N} for
// budget). PinnedBy/PinnedAt/Reason are attribution for the pin act itself.
type ControlPin struct {
	Value    json.RawMessage `json:"value"`
	PinnedBy string          `json:"pinned_by,omitempty"`
	PinnedAt time.Time       `json:"pinned_at,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}

// ControlChange is one entry in the control-field diff between the previous
// and the new registration of a workflow. Action is "changed", "added",
// "removed", or "pin_enforced" (the incoming value was discarded in favor of
// the operator pin).
type ControlChange struct {
	Target string          `json:"target"`
	Action string          `json:"action"`
	From   json.RawMessage `json:"from,omitempty"`
	To     json.RawMessage `json:"to,omitempty"`
}

// WorkflowControlEvent is one attribution-ledger entry: who performed which
// control-plane write on a workflow, when, moving it to which schema, with
// the control diff or pin detail attached.
type WorkflowControlEvent struct {
	ID        int64           `json:"id"`
	Project   string          `json:"project"`
	Name      string          `json:"name"`
	Action    string          `json:"action"`
	Actor     string          `json:"actor,omitempty"`
	SchemaRef string          `json:"schema_ref,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// DispatchInputSpec declares one dispatch-time input on a workflow registration.
// Name follows the same identifier rules as run-input keys. Required=true with
// no Default means the caller must provide a value or the dispatch is rejected
// at the API boundary (no fallback in the server, no implicit "main" guess).
// Default is filled in by the dispatch handler when the caller omits the key.
type DispatchInputSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
}

type WorkflowConstraints struct {
	Verification VerificationConstraints `json:"verification,omitempty"`
}

type VerificationConstraints struct {
	Shape    string `json:"shape,omitempty"`
	MaxCases int    `json:"max_cases,omitempty"`
}

type PhaseSpec struct {
	Name             string            `json:"name"`
	Kind             string            `json:"kind"`
	RunOn            string            `json:"run_on"`
	Purpose          string            `json:"purpose"`
	WorkflowFilename string            `json:"workflow_filename"`
	WorkflowRef      string            `json:"workflow_ref"`
	Inputs           map[string]string `json:"inputs"`
	Outputs          []string          `json:"outputs"`
	Requirements     map[string]any    `json:"requirements"`
	Verify           bool              `json:"verify"`
	RecyclePolicy    *RecyclePolicy    `json:"recycle_policy"`
	// Always is retired. It remains only so registration can reject stale
	// callers with a precise error instead of silently accepting the old
	// overloaded cleanup/review contract.
	Always                   bool            `json:"always,omitempty"`
	EvidenceVerificationGate bool            `json:"evidence_verification_gate"`
	DependsOn                []string        `json:"depends_on"`
	Jobs                     []RunnerJobSpec `json:"jobs"`
	// When is the phase-level run condition (whenexpr grammar). Evaluated
	// server-side at dispatch against the registration vars, the run's
	// dispatch inputs, and the closed run-fact set. False synthesizes a
	// durable skipped attempt instead of launching any job — zero compute,
	// GitHub-Actions `if:` parity. Empty means always run. Verification and
	// review-gate phases may not declare one: gates run.
	When string `json:"when,omitempty"`
	// SkipWhenPreserveTestEnv is retired. It remains only so registration
	// can reject stale callers with a precise error pointing at the `when`
	// replacement (`when: "${{ run.preserve_test_env }} == 'false'"`).
	SkipWhenPreserveTestEnv bool `json:"skip_when_preserve_test_env,omitempty"`
}

type RecyclePolicy struct {
	MaxAttempts int      `json:"max_attempts"`
	On          []string `json:"on"`
	LandsAt     string   `json:"lands_at"`
}

type RunnerJobSpec struct {
	ID               string               `json:"id"`
	Name             *string              `json:"name"`
	Primitive        string               `json:"primitive,omitempty"`
	Image            string               `json:"image"`
	Command          []string             `json:"command"`
	Args             []string             `json:"args"`
	Env              map[string]string    `json:"env"`
	Steps            []RunnerStepSpec     `json:"steps"`
	TimeoutSeconds   *int                 `json:"timeout_seconds"`
	Managed          bool                 `json:"managed,omitempty"`
	Checkout         *RunnerCheckoutSpec  `json:"checkout,omitempty"`
	ExtraCheckouts   []RunnerCheckoutSpec `json:"extra_checkouts,omitempty"`
	WorkingDirectory string               `json:"working_directory,omitempty"`
	Shell            string               `json:"shell,omitempty"`
	// When is the job-level run condition (whenexpr grammar). Evaluated
	// server-side at phase dispatch; false means this job's Kubernetes Job
	// is never created — the platform writes a synthesized `skipped` job
	// completion and skipped job/step execution records instead, and the
	// phase completes on the launched siblings alone. The declared-but-
	// skipped job keeps the workflow shape total and renders the toggle.
	When string `json:"when,omitempty"`
	// Tools is the per-job runner MCP allow-list: the exact set of structured
	// tools the job's agent may call. When non-empty, the launcher runs the
	// scoped runner MCP server as a sidecar exposing only these tools and shares
	// the run workspace with it. Empty means no sidecar and no runner tools —
	// the default, leaving the pod spec unchanged.
	Tools []string `json:"tools,omitempty"`
}

type RunnerStepSpec struct {
	Slug             string            `json:"slug"`
	Title            *string           `json:"title"`
	Type             string            `json:"type,omitempty"`
	Run              string            `json:"run,omitempty"`
	Agent            *AgentStepSpec    `json:"agent,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Group            string            `json:"group,omitempty"`
	GroupTitle       *string           `json:"group_title,omitempty"`
	DynamicGroup     *StepDynamicGroup `json:"dynamic_group,omitempty"`
}

type StepDynamicGroup struct {
	MaxItems  int    `json:"max_items,omitempty"`
	ItemLabel string `json:"item_label,omitempty"`
}

type RunnerCheckoutSpec struct {
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
}

type AgentStepSpec struct {
	Slot       string `json:"slot,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	PromptFile string `json:"prompt_file,omitempty"`
	// GithubToken opts the managed agent step into the repo-scoped GitHub
	// token contract (docs/github-agent-push-policy.md): the runner mints
	// per run and hands the agent GITHUB_TOKEN_FILE. Implementation steps
	// only — read-only stages must not set it.
	GithubToken bool `json:"github_token,omitempty"`
}

// PrPrimitive declares the workflow's PR recycle policy. Every Glimmung
// workflow ends in a human-reviewed PR — there is no opt-out — so the
// previous "enabled" toggle has been removed per migration-policy: there
// was no documented product scenario for a workflow without a PR, so the
// flag was a compatibility surface rather than a real choice.
type PrPrimitive struct {
	RecyclePolicy *RecyclePolicy `json:"recycle_policy"`
}

func listProjects(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "read store not configured")
			return
		}
		limit, ok := parseLimit(w, r)
		if !ok {
			return
		}
		rows, err := store.ListProjects(r.Context())
		if err != nil {
			writeInternalError(w, r, err, "list projects failed")
			return
		}

		nameNeedle := strings.ToLower(r.URL.Query().Get("name"))
		repoNeedle := strings.ToLower(r.URL.Query().Get("github_repo"))
		filtered := make([]Project, 0, len(rows))
		for _, row := range rows {
			if nameNeedle != "" && !strings.Contains(strings.ToLower(row.Name), nameNeedle) {
				continue
			}
			if repoNeedle != "" && !strings.Contains(strings.ToLower(row.GitHubRepo), repoNeedle) {
				continue
			}
			filtered = append(filtered, row)
			if limit != nil && len(filtered) >= *limit {
				break
			}
		}
		writeJSON(w, http.StatusOK, filtered)
	}
}

func listWorkflows(store ReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeProblem(w, http.StatusServiceUnavailable, "read store not configured")
			return
		}
		limit, ok := parseLimit(w, r)
		if !ok {
			return
		}
		rows, err := store.ListWorkflows(r.Context())
		if err != nil {
			writeInternalError(w, r, err, "list workflows failed")
			return
		}

		project := r.URL.Query().Get("project")
		nameNeedle := strings.ToLower(r.URL.Query().Get("name"))
		filtered := make([]Workflow, 0, len(rows))
		for _, row := range rows {
			if project != "" && row.Project != project {
				continue
			}
			if nameNeedle != "" && !strings.Contains(strings.ToLower(row.Name), nameNeedle) {
				continue
			}
			filtered = append(filtered, row)
			if limit != nil && len(filtered) >= *limit {
				break
			}
		}
		writeJSON(w, http.StatusOK, filtered)
	}
}

func parseLimit(w http.ResponseWriter, r *http.Request) (*int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return nil, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 500 {
		writeProblem(w, http.StatusBadRequest, "limit must be between 1 and 500")
		return nil, false
	}
	return &limit, true
}

// writeProblem writes a 4xx JSON problem response. The one allowed 5xx
// use of writeProblem is http.StatusServiceUnavailable for
// configuration-absence callsites ("X store not configured"); those
// genuinely have no operational signal worth logging — the deploy
// didn't wire a dependency, the boot will be retried, the error class
// is "ops misconfig" not "runtime saturation". Every other 5xx must go
// through writeInternalError (for unexpected errors) or
// writeUnavailable (for deliberate operational 503s). The migration
// guard at scripts/check-handler-5xx-migration.mjs fails CI on
// writeProblem with StatusInternalServerError; the companion guard at
// scripts/check-503-observability.mjs enforces that any
// writeProblem-503 carries a "not configured" literal and that
// operational 503s use writeUnavailable.
//
// Removing the swallow path is the whole point of glimmung#514: without
// the err the only signal a 500 leaves is the abstract `detail` body
// the SPA prints, and the actual cause is unrecoverable. See
// docs/quality-timeframes.md for why observability is gating scope.
func writeProblem(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"detail": message})
}

// writeInternalError writes a 500 JSON problem response and emits a
// structured slog.Error capturing the request method, the registered
// route pattern (r.Pattern, Go 1.22+ ServeMux), the public summary, and
// the underlying error. The body remains the abstract `summary` so the
// public API surface is unchanged; the err is preserved in logs only.
//
// Callers must supply an err that explains the 5xx. If a 5xx has no
// underlying error to log, the right shape is usually a different status
// (404, 409, 422) — investigate before defaulting to writeInternalError
// with a synthesized error.
func writeInternalError(w http.ResponseWriter, r *http.Request, err error, summary string) {
	route := r.Pattern
	if route == "" {
		route = "(unmatched)"
	}
	slog.Error("handler returned 5xx",
		"method", r.Method,
		"route", route,
		"summary", summary,
		"err", err,
	)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": summary})
}

// writeUnavailable writes a 503 JSON problem response and emits a
// structured slog.Warn capturing the request method, the registered
// route pattern, the public summary, and a short reason enum. Also
// increments the glimmung_unavailable_total counter so deliberate
// 503s are observable on a dashboard — the operator's question is
// "is the cluster saturated?" not "what was the err?".
//
// Use this for runtime, retryable 503s: saturation (no free slots),
// transient dependency unavailability with a backoff signal, etc.
// reason is a closed-enum string the callsite picks at compile time
// (e.g. "no_prepared_test_slot") so the metric label stays bounded.
//
// For 5xx caused by an unexpected error, use writeInternalError
// instead. For configuration-absence 503s ("X store not configured"),
// continue using writeProblem — those have no operational signal.
func writeUnavailable(w http.ResponseWriter, r *http.Request, summary, reason string) {
	route := r.Pattern
	if route == "" {
		route = "(unmatched)"
	}
	slog.Warn("handler returned 503",
		"method", r.Method,
		"route", route,
		"summary", summary,
		"reason", reason,
	)
	metrics.RecordUnavailable(route, reason)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"detail": summary})
}

func stringPtr(value string) *string {
	return &value
}
