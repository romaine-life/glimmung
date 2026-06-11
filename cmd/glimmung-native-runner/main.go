package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/romaine-life/glimmung/internal/domain/agentcost"
	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
	"github.com/romaine-life/glimmung/internal/domain/decision"
	"github.com/romaine-life/glimmung/internal/domain/innerjob"
)

const (
	defaultWorkspace        = "/workspace"
	defaultAttemptTokenPath = "/var/run/glimmung/attempt-token"
	maxForwardedLogBytes    = 64 * 1024
	maxScannerTokenBytes    = 1024 * 1024
	evidenceTarStartMarker  = "===EVIDENCE-TAR-START==="
	evidenceTarEndMarker    = "===EVIDENCE-TAR-END==="
	logEventPostTimeout     = 5 * time.Second
	// shutdownCompleteBudget is the time we reserve from the kubelet's
	// terminationGracePeriodSeconds (default 30s) to deliver the
	// timed_out /completed callback when we receive SIGTERM. The
	// remainder of the grace period covers in-flight HTTP writes and
	// child-process teardown.
	shutdownCompleteBudget = 20 * time.Second
)

type jobSpec struct {
	ID               string            `json:"id"`
	Env              map[string]string `json:"env"`
	Steps            []stepSpec        `json:"steps"`
	Checkout         *checkoutSpec     `json:"checkout"`
	ExtraCheckouts   []checkoutSpec    `json:"extra_checkouts"`
	WorkingDirectory string            `json:"working_directory"`
	Shell            string            `json:"shell"`
}

type stepSpec struct {
	Slug             string            `json:"slug"`
	Type             string            `json:"type"`
	Run              string            `json:"run"`
	Agent            *agentStepSpec    `json:"agent"`
	Shell            string            `json:"shell"`
	WorkingDirectory string            `json:"working_directory"`
	Env              map[string]string `json:"env"`
	Group            string            `json:"group"`
	GroupTitle       string            `json:"group_title"`
	DynamicGroup     *dynamicGroupSpec `json:"dynamic_group"`
}

type dynamicGroupSpec struct {
	MaxItems  int    `json:"max_items"`
	ItemLabel string `json:"item_label"`
}

type agentStepSpec struct {
	Slot       string `json:"slot"`
	Prompt     string `json:"prompt"`
	PromptFile string `json:"prompt_file"`
}

type checkoutSpec struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

type runnerConfig struct {
	Job                 jobSpec
	JobID               string
	AttemptIndex        *int
	EventsURL           string
	CompletedURL        string
	GitHubTokenURL      string
	GitHubAgentTokenURL string
	AttemptToken        string
	Workspace           string
	AgentRuntime        agentruntime.Snapshot
}

type nativeRunner struct {
	cfg              runnerConfig
	client           *http.Client
	seq              int
	outputs          map[string]string
	completion       completionMetadata
	caseCompletions  []dynamicCaseCompletion
	githubTokenCache *githubTokenResult
	mu               sync.Mutex
	costUSD          float64
}

type nativeEventRequest struct {
	JobID        string         `json:"job_id"`
	Seq          int            `json:"seq"`
	Event        string         `json:"event"`
	AttemptIndex *int           `json:"attempt_index,omitempty"`
	StepSlug     *string        `json:"step_slug,omitempty"`
	Message      *string        `json:"message,omitempty"`
	ExitCode     *int           `json:"exit_code,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type completedRequest struct {
	JobID               string             `json:"job_id"`
	Conclusion          string             `json:"conclusion"`
	AttemptIndex        *int               `json:"attempt_index,omitempty"`
	CostUSD             float64            `json:"cost_usd,omitempty"`
	Verification        map[string]any     `json:"verification,omitempty"`
	Evidence            []evidenceArtifact `json:"evidence,omitempty"`
	ScreenshotsMarkdown *string            `json:"screenshots_markdown,omitempty"`
	SummaryMarkdown     *string            `json:"summary_markdown,omitempty"`
	Outputs             map[string]string  `json:"outputs"`
}

type dynamicVerificationFailure struct {
	Status string
	Reason string
}

func (e dynamicVerificationFailure) Error() string {
	status := firstNonEmpty(strings.TrimSpace(e.Status), "fail")
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		return "dynamic verification case reported status=" + status
	}
	return "dynamic verification case reported status=" + status + ": " + reason
}

type completionMetadata struct {
	Verification        map[string]any     `json:"verification"`
	Evidence            []evidenceArtifact `json:"evidence"`
	ScreenshotsMarkdown string             `json:"screenshots_markdown"`
	SummaryMarkdown     string             `json:"summary_markdown"`
}

type evidenceArtifact struct {
	Kind         string `json:"kind"`
	Ref          string `json:"ref"`
	Label        string `json:"label"`
	URL          string `json:"url,omitempty"`
	ArtifactPath string `json:"artifact_path,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	DurationMS   int    `json:"duration_ms,omitempty"`
}

type dynamicCaseCompletion struct {
	Index        int
	Label        string
	Verification map[string]any
	Evidence     []evidenceArtifact
}

type githubTokenResult struct {
	Token string `json:"token"`
}

func main() {
	cfg, err := runnerConfigFromEnv()
	if err != nil {
		log.Printf("configure runner: %v", err)
		os.Exit(1)
	}
	r := &nativeRunner{
		cfg:     cfg,
		client:  &http.Client{Timeout: 30 * time.Second},
		outputs: map[string]string{},
	}
	// Catch SIGTERM (kubelet activeDeadlineSeconds, pod eviction, node
	// drain) and SIGINT (local dev) so we can report a terminal
	// /completed callback before kubelet's SIGKILL lands.
	//
	// Without this, a pod killed mid-step delivers no callback at all
	// and the run sits in_progress forever from glimmung's side.
	// ambience#170/runs/1.1 is the canonical incident; the run-execution
	// reconciler is the safety net for the truly violent paths
	// (OOMKilled, node loss, eviction-without-grace) that fire SIGKILL
	// straight away and skip this handler entirely.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	if err := r.run(signalCtx); err != nil {
		log.Printf("native runner failed: %v", err)
		os.Exit(1)
	}
}

func runnerConfigFromEnv() (runnerConfig, error) {
	rawSpec := strings.TrimSpace(os.Getenv("GLIMMUNG_RUNNER_JOB_SPEC"))
	if rawSpec == "" {
		return runnerConfig{}, errors.New("GLIMMUNG_RUNNER_JOB_SPEC required")
	}
	var job jobSpec
	if err := json.Unmarshal([]byte(rawSpec), &job); err != nil {
		return runnerConfig{}, fmt.Errorf("decode GLIMMUNG_RUNNER_JOB_SPEC: %w", err)
	}
	jobID := firstNonEmpty(os.Getenv("GLIMMUNG_JOB_ID"), job.ID)
	if strings.TrimSpace(jobID) == "" {
		return runnerConfig{}, errors.New("GLIMMUNG_JOB_ID required")
	}
	var attemptIndex *int
	if raw := strings.TrimSpace(os.Getenv("GLIMMUNG_ATTEMPT_INDEX")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return runnerConfig{}, fmt.Errorf("GLIMMUNG_ATTEMPT_INDEX must be an integer: %w", err)
		}
		attemptIndex = &parsed
	}
	var runtime agentruntime.Snapshot
	if raw := strings.TrimSpace(os.Getenv("GLIMMUNG_AGENT_RUNTIME_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &runtime); err != nil {
			return runnerConfig{}, fmt.Errorf("decode GLIMMUNG_AGENT_RUNTIME_JSON: %w", err)
		}
	}
	token := strings.TrimSpace(os.Getenv("GLIMMUNG_ATTEMPT_TOKEN"))
	if token == "" {
		fromFile, err := os.ReadFile(defaultAttemptTokenPath)
		if err == nil {
			token = strings.TrimSpace(string(fromFile))
		}
	}
	return runnerConfig{
		Job:                 job,
		JobID:               jobID,
		AttemptIndex:        attemptIndex,
		EventsURL:           strings.TrimSpace(os.Getenv("GLIMMUNG_EVENTS_URL")),
		CompletedURL:        strings.TrimSpace(os.Getenv("GLIMMUNG_COMPLETED_URL")),
		GitHubTokenURL:      strings.TrimSpace(os.Getenv("GLIMMUNG_GITHUB_TOKEN_URL")),
		GitHubAgentTokenURL: strings.TrimSpace(os.Getenv("GLIMMUNG_GITHUB_AGENT_TOKEN_URL")),
		AttemptToken:        token,
		Workspace:           firstNonEmpty(os.Getenv("GLIMMUNG_WORKSPACE"), defaultWorkspace),
		AgentRuntime:        runtime,
	}, nil
}

func (r *nativeRunner) run(ctx context.Context) error {
	if err := os.MkdirAll(r.cfg.Workspace, 0o755); err != nil {
		_ = r.completeOrShutdown(ctx, "failure", "create workspace: "+err.Error())
		return err
	}
	if err := r.prepareCheckouts(ctx); err != nil {
		if r.shutdownRequested(ctx) {
			return r.completeShutdown(ctx, "runner received shutdown during checkout: "+err.Error())
		}
		_ = r.postEvent(ctx, "runner_failed", nil, "checkout failed: "+err.Error(), nil, nil)
		_ = r.complete(ctx, "failure", "checkout failed: "+err.Error())
		return err
	}
	for i := 0; i < len(r.cfg.Job.Steps); i++ {
		step := r.cfg.Job.Steps[i]
		if block, next, ok := dynamicStepBlock(r.cfg.Job.Steps, i); ok {
			if err := r.runDynamicStepBlock(ctx, block); err != nil {
				if r.shutdownRequested(ctx) {
					return r.completeShutdown(ctx, "runner received shutdown during dynamic step block: "+err.Error())
				}
				_ = r.complete(ctx, "failure", err.Error())
				return err
			}
			i = next - 1
			if reason := r.requestedAbortReason(); reason != "" {
				msg := fmt.Sprintf("dynamic step block %q requested run abort: %s", block[0].Group, reason)
				return r.complete(ctx, decision.ConclusionAborted, msg)
			}
			continue
		}
		if strings.TrimSpace(step.Type) == "" {
			step.Type = "run"
		}
		if step.Type != "run" && step.Type != "agent" {
			err := fmt.Errorf("step %q uses unsupported type %q", step.Slug, step.Type)
			_ = r.complete(ctx, "failure", err.Error())
			return err
		}
		if err := r.runStep(ctx, step); err != nil {
			if r.shutdownRequested(ctx) {
				slug := strings.TrimSpace(step.Slug)
				return r.completeShutdown(
					ctx,
					fmt.Sprintf("runner received shutdown during step %q: %v", slug, err),
				)
			}
			_ = r.complete(ctx, "failure", err.Error())
			return err
		}
		// A step can short-circuit the whole phase by emitting a
		// non-empty `abort_reason` phase output (the spirelens env-prep
		// guards do this: host asleep, unexpected mod on disk). This is a
		// fail-closed signal — the remaining steps in the phase must NOT
		// run. We stop here and report a terminal abort completion so the
		// decision engine routes the run to teardown-then-abort with the
		// phase's own reason, rather than letting later steps execute as
		// if nothing happened.
		if reason := r.requestedAbortReason(); reason != "" {
			slug := strings.TrimSpace(step.Slug)
			msg := fmt.Sprintf("step %q requested run abort: %s", slug, reason)
			return r.complete(ctx, decision.ConclusionAborted, msg)
		}
	}
	return r.complete(ctx, "success", "completed")
}

func dynamicStepBlock(steps []stepSpec, start int) ([]stepSpec, int, bool) {
	if start < 0 || start >= len(steps) || steps[start].DynamicGroup == nil {
		return nil, start, false
	}
	group := strings.TrimSpace(steps[start].Group)
	if group == "" {
		return nil, start, false
	}
	end := start
	for end < len(steps) {
		step := steps[end]
		if step.DynamicGroup == nil || strings.TrimSpace(step.Group) != group {
			break
		}
		end++
	}
	return steps[start:end], end, true
}

type dynamicCase struct {
	Index int
	Raw   string
	Label string
}

func (r *nativeRunner) runDynamicStepBlock(ctx context.Context, block []stepSpec) error {
	if len(block) == 0 {
		return nil
	}
	group := strings.TrimSpace(block[0].Group)
	groupTitle := firstNonEmpty(strings.TrimSpace(block[0].GroupTitle), group)
	maxItems := block[0].DynamicGroup.MaxItems
	if maxItems <= 0 {
		return fmt.Errorf("dynamic group %q max_items must be positive", group)
	}
	cases, err := r.dynamicCasesForGroup(group, maxItems)
	if err != nil {
		return err
	}
	plannedSteps := concreteDynamicSteps(block, cases, group, maxItems)
	if err := r.postEvent(ctx, "dynamic_group_expanded", nil, "", nil, map[string]any{
		"group":       group,
		"group_title": groupTitle,
		"item_count":  len(cases),
		"max_items":   maxItems,
		"item_label":  firstNonEmpty(strings.TrimSpace(block[0].DynamicGroup.ItemLabel), "item"),
		"steps":       dynamicPlannedStepMetadata(plannedSteps),
	}); err != nil {
		return err
	}
	if len(cases) == 0 {
		slug := safeStepSlug(group) + "-no-cases"
		skipped := stepSpec{
			Slug:       slug,
			Group:      group,
			GroupTitle: groupTitle,
		}
		if err := r.postEvent(ctx, "step_skipped", &slug, "no dynamic test cases generated", nil, stepEventMetadata(skipped)); err != nil {
			return err
		}
		return nil
	}
	lastCaseIndex := 0
	for _, step := range plannedSteps {
		caseIndex, _ := strconv.Atoi(strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_INDEX"]))
		if caseIndex != lastCaseIndex && lastCaseIndex > 0 && r.dynamicCaseFailed(lastCaseIndex) {
			msg := fmt.Sprintf("dynamic group %q stopped after case %02d failed", group, lastCaseIndex)
			if err := r.postEvent(ctx, "dynamic_group_stopped", nil, msg, nil, map[string]any{
				"group":             group,
				"group_title":       groupTitle,
				"failed_case_index": lastCaseIndex,
				"reason":            "case_failed",
			}); err != nil {
				return err
			}
			return nil
		}
		lastCaseIndex = caseIndex
		if strings.TrimSpace(step.Type) == "" {
			step.Type = "run"
		}
		if err := r.runStep(ctx, step); err != nil {
			var verificationFailure dynamicVerificationFailure
			if errors.As(err, &verificationFailure) {
				msg := fmt.Sprintf("dynamic group %q stopped after case %02d failed", group, caseIndex)
				if postErr := r.postEvent(ctx, "dynamic_group_stopped", nil, msg, nil, map[string]any{
					"group":             group,
					"group_title":       groupTitle,
					"failed_case_index": caseIndex,
					"reason":            "case_failed",
				}); postErr != nil {
					return postErr
				}
			}
			return err
		}
		if reason := r.requestedAbortReason(); reason != "" {
			return nil
		}
	}
	return nil
}

func concreteDynamicSteps(block []stepSpec, cases []dynamicCase, group string, maxItems int) []stepSpec {
	if len(block) == 0 || len(cases) == 0 {
		return nil
	}
	itemLabel := firstNonEmpty(strings.TrimSpace(block[0].DynamicGroup.ItemLabel), "case")
	steps := make([]stepSpec, 0, len(block)*len(cases))
	for _, tc := range cases {
		caseGroup := fmt.Sprintf("%s/case-%02d", group, tc.Index)
		caseTitle := firstNonEmpty(tc.Label, fmt.Sprintf("%s %02d", itemLabel, tc.Index))
		for _, template := range block {
			step := template
			step.Slug = fmt.Sprintf("%s-case-%02d", template.Slug, tc.Index)
			step.Group = caseGroup
			step.GroupTitle = caseTitle
			step.DynamicGroup = nil
			step.Env = mergeStringMaps(template.Env, map[string]string{
				"GLIMMUNG_DYNAMIC_GROUP":            group,
				"GLIMMUNG_DYNAMIC_CASE_INDEX":       strconv.Itoa(tc.Index),
				"GLIMMUNG_DYNAMIC_CASE_COUNT":       strconv.Itoa(len(cases)),
				"GLIMMUNG_DYNAMIC_CASE_LABEL":       caseTitle,
				"GLIMMUNG_DYNAMIC_CASE_JSON":        tc.Raw,
				"GLIMMUNG_DYNAMIC_TEMPLATE_STEP":    template.Slug,
				"GLIMMUNG_DYNAMIC_TEMPLATE_GROUP":   group,
				"GLIMMUNG_DYNAMIC_TEMPLATE_MAX":     strconv.Itoa(maxItems),
				"GLIMMUNG_DYNAMIC_TEMPLATE_ITEM_ID": strconv.Itoa(tc.Index),
			})
			if strings.TrimSpace(step.Type) == "" {
				step.Type = "run"
			}
			steps = append(steps, step)
		}
	}
	return steps
}

func dynamicPlannedStepMetadata(steps []stepSpec) []map[string]any {
	out := make([]map[string]any, 0, len(steps))
	for _, step := range steps {
		item := map[string]any{
			"slug":  step.Slug,
			"title": step.Slug,
			"type":  firstNonEmpty(strings.TrimSpace(step.Type), "run"),
		}
		if group := strings.TrimSpace(step.Group); group != "" {
			item["group"] = group
		}
		if groupTitle := strings.TrimSpace(step.GroupTitle); groupTitle != "" {
			item["group_title"] = groupTitle
		}
		out = append(out, item)
	}
	return out
}

func (r *nativeRunner) dynamicCasesForGroup(group string, maxItems int) ([]dynamicCase, error) {
	keys := dynamicCaseOutputKeys(group)
	for _, key := range keys.jsonKeys {
		raw := strings.TrimSpace(r.outputs[key])
		if raw == "" {
			continue
		}
		cases, err := parseDynamicCasesJSON(raw, maxItems)
		if err != nil {
			return nil, fmt.Errorf("dynamic group %q output %q is invalid: %w", group, key, err)
		}
		return cases, nil
	}
	for _, key := range keys.countKeys {
		raw := strings.TrimSpace(r.outputs[key])
		if raw == "" {
			continue
		}
		count, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("dynamic group %q output %q must be an integer: %w", group, key, err)
		}
		if count < 0 || count > maxItems {
			return nil, fmt.Errorf("dynamic group %q output %q=%d is outside [0,%d]", group, key, count, maxItems)
		}
		cases := make([]dynamicCase, 0, count)
		for i := 1; i <= count; i++ {
			cases = append(cases, dynamicCase{Index: i, Raw: "{}", Label: fmt.Sprintf("case %02d", i)})
		}
		return cases, nil
	}
	return nil, fmt.Errorf("dynamic group %q requires an earlier step to publish one of %s or %s", group, strings.Join(keys.jsonKeys, ", "), strings.Join(keys.countKeys, ", "))
}

type dynamicCaseKeys struct {
	jsonKeys  []string
	countKeys []string
}

func dynamicCaseOutputKeys(group string) dynamicCaseKeys {
	normalized := normalizeOutputKey(group)
	keys := dynamicCaseKeys{
		jsonKeys: []string{
			normalized + "_json",
			normalized + "_cases_json",
			"glimmung_dynamic_" + normalized + "_json",
			"glimmung_dynamic_" + normalized + "_cases_json",
		},
		countKeys: []string{
			normalized + "_count",
			normalized + "_cases_count",
			"glimmung_dynamic_" + normalized + "_count",
			"glimmung_dynamic_" + normalized + "_cases_count",
		},
	}
	if normalized != "test_cases" {
		keys.jsonKeys = append([]string{"test_cases_json"}, keys.jsonKeys...)
		keys.countKeys = append([]string{"test_cases_count"}, keys.countKeys...)
	}
	return keys
}

func normalizeOutputKey(value string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func parseDynamicCasesJSON(raw string, maxItems int) ([]dynamicCase, error) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, err
	}
	items, ok := decoded.([]any)
	if !ok {
		if obj, ok := decoded.(map[string]any); ok {
			if nested, nestedOK := obj["cases"].([]any); nestedOK {
				items = nested
				ok = true
			}
		}
	}
	if !ok {
		return nil, errors.New("expected JSON array or object with cases array")
	}
	if len(items) > maxItems {
		return nil, fmt.Errorf("contains %d cases, maximum is %d", len(items), maxItems)
	}
	cases := make([]dynamicCase, 0, len(items))
	for i, item := range items {
		payload, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		cases = append(cases, dynamicCase{
			Index: i + 1,
			Raw:   string(payload),
			Label: dynamicCaseLabel(item, i+1),
		})
	}
	return cases, nil
}

func dynamicCaseLabel(item any, index int) string {
	obj, ok := item.(map[string]any)
	if !ok {
		return fmt.Sprintf("case %02d", index)
	}
	for _, key := range []string{"label", "title", "name", "id"} {
		if value := strings.TrimSpace(fmt.Sprint(obj[key])); value != "" && value != "<nil>" {
			return value
		}
	}
	return fmt.Sprintf("case %02d", index)
}

// requestedAbortReason returns the phase's `abort_reason` output if a step
// has set it to a non-empty value, signalling a fail-closed abort. Outputs
// are only written on the main goroutine (publishOutputs runs synchronously
// after each step), so an unlocked read here is race-free.
func (r *nativeRunner) requestedAbortReason() string {
	return strings.TrimSpace(r.outputs[decision.AbortReasonOutputKey])
}

// shutdownRequested reports whether the supplied (signal-aware) context
// has been cancelled. NotifyContext cancels its returned context on the
// first SIGTERM/SIGINT, so this is the precise distinction between
// "child step failed on its own" and "we're being torn down."
func (r *nativeRunner) shutdownRequested(ctx context.Context) bool {
	return ctx.Err() != nil
}

// completeShutdown posts a terminal /completed callback with
// conclusion=timed_out before the kubelet sends SIGKILL. The original
// context has been cancelled by signal.NotifyContext, so we open a
// fresh background context with a tight budget that fits inside the
// pod's terminationGracePeriodSeconds — the in-flight HTTP write is
// the only thing we promise to finish on the way out.
//
// Always returns a non-nil error so r.run propagates the shutdown up
// to main() which exits non-zero. The Job's pod is already terminating;
// the exit code is mostly cosmetic but a clean non-zero is correct
// shape for "the runner was killed before it could finish its work."
func (r *nativeRunner) completeShutdown(_ context.Context, summary string) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownCompleteBudget)
	defer cancel()
	_ = r.postEvent(shutdownCtx, "runner_failed", nil, summary, nil, nil)
	if err := r.complete(shutdownCtx, "timed_out", summary); err != nil {
		log.Printf("shutdown completion callback failed: %v", err)
		return fmt.Errorf("shutdown completion callback failed: %w", err)
	}
	log.Printf("shutdown completion callback delivered: %s", summary)
	return errors.New(summary)
}

// completeOrShutdown picks the right conclusion based on whether the
// supplied context has already been cancelled. Used for early-init
// failures where we don't yet know if SIGTERM raced the failure.
func (r *nativeRunner) completeOrShutdown(ctx context.Context, conclusion, summary string) error {
	if r.shutdownRequested(ctx) {
		return r.completeShutdown(ctx, summary)
	}
	return r.complete(ctx, conclusion, summary)
}

func (r *nativeRunner) runStep(ctx context.Context, step stepSpec) error {
	slug := strings.TrimSpace(step.Slug)
	if slug == "" {
		return errors.New("step slug required")
	}
	if err := r.postEvent(ctx, "step_started", &slug, "", nil, stepEventMetadata(step)); err != nil {
		return err
	}
	outputFile := filepath.Join(os.TempDir(), "glimmung-output-"+slug+".txt")
	completionFile := filepath.Join(os.TempDir(), "glimmung-completion-"+slug+".json")
	_ = os.Remove(outputFile)
	_ = os.Remove(completionFile)
	var exitCode int
	var execErr error
	switch strings.TrimSpace(step.Type) {
	case "", "run":
		exitCode, execErr = r.executeStep(ctx, step, outputFile, completionFile)
	case "agent":
		exitCode, execErr = r.executeAgentStep(ctx, step, outputFile, completionFile)
	default:
		exitCode, execErr = 1, fmt.Errorf("unsupported step type %q", step.Type)
	}
	outputs, outputErr := parseOutputFile(outputFile)
	if outputErr == nil {
		outputErr = r.publishOutputs(ctx, slug, outputs)
	}
	if completionErr := r.collectCompletionMetadata(completionFile, step); outputErr == nil && completionErr != nil {
		outputErr = completionErr
	}
	if execErr != nil {
		msg := fmt.Sprintf("step %s exited with code %d", slug, exitCode)
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exitCode, stepEventMetadata(step))
		return fmt.Errorf("%s: %w", msg, execErr)
	}
	if outputErr != nil {
		exit := 1
		msg := "step " + slug + " output error: " + outputErr.Error()
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exit, stepEventMetadata(step))
		return errors.New(msg)
	}
	if status, reason, failed := r.dynamicCaseFailureForStep(step); failed {
		exit := 1
		msg := fmt.Sprintf("verification case %s reported status=%s", dynamicCaseStepLabel(step), status)
		if strings.TrimSpace(reason) != "" {
			msg += ": " + strings.TrimSpace(reason)
		}
		metadata := stepEventMetadata(step)
		metadata["verification_status"] = status
		if strings.TrimSpace(reason) != "" {
			metadata["verification_reason"] = strings.TrimSpace(reason)
		}
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exit, metadata)
		return dynamicVerificationFailure{Status: status, Reason: reason}
	}
	if reason := r.requestedAbortReason(); reason != "" {
		msg := fmt.Sprintf("step %q requested run abort: %s", slug, reason)
		metadata := stepEventMetadata(step)
		metadata["abort_reason"] = reason
		metadata["abort_scope"] = "run_after_cleanup"
		if err := r.postEvent(ctx, "step_aborted", &slug, msg, nil, metadata); err != nil {
			return err
		}
		return nil
	}
	if err := r.postEvent(ctx, "step_completed", &slug, "", &exitCode, stepEventMetadata(step)); err != nil {
		return err
	}
	return nil
}

func stepEventMetadata(step stepSpec) map[string]any {
	metadata := map[string]any{}
	if group := strings.TrimSpace(step.Group); group != "" {
		metadata["group"] = group
	}
	if title := strings.TrimSpace(step.GroupTitle); title != "" {
		metadata["group_title"] = title
	}
	if step.DynamicGroup != nil {
		metadata["dynamic_group"] = step.DynamicGroup
	}
	return metadata
}

func (r *nativeRunner) executeStep(ctx context.Context, step stepSpec, outputFile, completionFile string) (int, error) {
	return r.executeStepWithEnv(ctx, step, outputFile, completionFile, os.Environ())
}

func (r *nativeRunner) executeStepWithEnv(ctx context.Context, step stepSpec, outputFile, completionFile string, baseEnv []string) (int, error) {
	workdir := firstNonEmpty(step.WorkingDirectory, r.cfg.Job.WorkingDirectory, r.cfg.Workspace)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return 1, err
	}
	name, args := shellCommand(firstNonEmpty(step.Shell, r.cfg.Job.Shell), step.Run)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workdir
	cmd.Env = mergedEnv(baseEnv, r.cfg.Job.Env, step.Env, map[string]string{
		"GLIMMUNG_MANAGED_RUNNER":  "1",
		"GLIMMUNG_OUTPUT_FILE":     outputFile,
		"GLIMMUNG_COMPLETION_FILE": completionFile,
		"GLIMMUNG_STEP_SLUG":       step.Slug,
	})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go r.streamLogs(ctx, &wg, step.Slug, "stdout", stdout)
	go r.streamLogs(ctx, &wg, step.Slug, "stderr", stderr)
	wg.Wait()
	waitErr := cmd.Wait()
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), waitErr
	}
	return 1, waitErr
}

func (r *nativeRunner) executeAgentStep(ctx context.Context, step stepSpec, outputFile, completionFile string) (int, error) {
	workdir := firstNonEmpty(step.WorkingDirectory, r.cfg.Job.WorkingDirectory, r.cfg.Workspace)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return 1, err
	}
	spec := step.Agent
	if spec == nil {
		spec = &agentStepSpec{}
	}
	slot := strings.TrimSpace(spec.Slot)
	if slot == "" {
		slot = agentruntime.DefaultSlot
	}
	profile, ok := r.cfg.AgentRuntime.ProfileForSlot(slot)
	if !ok {
		return 1, fmt.Errorf("agent step %q slot %q has no resolved runtime profile", step.Slug, slot)
	}
	if err := r.postEvent(ctx, "agent_runtime_selected", &step.Slug, "", nil, map[string]any{
		"slot":             slot,
		"profile_id":       profile.ProfileID,
		"provider":         profile.Provider,
		"model":            profile.Model,
		"reasoning_effort": profile.ReasoningEffort,
		"source":           profile.Source,
	}); err != nil {
		return 1, err
	}
	prompt, err := r.agentPrompt(workdir, step, *spec, slot, profile)
	if err != nil {
		return 1, err
	}
	promptHandle, err := os.CreateTemp("", "glimmung-agent-prompt-*.md")
	if err != nil {
		return 1, err
	}
	promptFile := promptHandle.Name()
	if _, err := promptHandle.WriteString(prompt); err != nil {
		_ = promptHandle.Close()
		return 1, err
	}
	if err := promptHandle.Close(); err != nil {
		return 1, err
	}
	defer os.Remove(promptFile)
	if err := installAgentPostCommitReminder(ctx, workdir); err != nil {
		return 1, err
	}
	runStep := step
	runStep.Type = "run"
	runStep.Run, err = agentRunScript(profile, workdir, promptFile, completionFile)
	if err != nil {
		return 1, err
	}
	runStep.Shell = "bash"
	return r.executeStepWithEnv(ctx, runStep, outputFile, completionFile, agentStepBaseEnv(os.Environ()))
}

func installAgentPostCommitReminder(ctx context.Context, workdir string) error {
	repoRoot, err := runOutput(ctx, workdir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	repoRoot = strings.TrimSpace(repoRoot)
	if repoRoot == "" {
		return nil
	}
	gitDir, err := runOutput(ctx, repoRoot, "git", "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("resolve git dir for agent post-commit hook: %w", err)
	}
	gitDir = strings.TrimSpace(gitDir)
	if gitDir == "" {
		return nil
	}
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create git hooks dir: %w", err)
	}
	hookPath := filepath.Join(hooksDir, "post-commit")
	const marker = "GLIMMUNG MANAGED AGENT POST-COMMIT REMINDER"
	hook := agentPostCommitReminderHook(marker)
	if existing, err := os.ReadFile(hookPath); err == nil && !strings.Contains(string(existing), marker) {
		return fmt.Errorf("refusing to replace existing post-commit hook: %s", hookPath)
	}
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		return fmt.Errorf("write agent post-commit hook: %w", err)
	}
	return nil
}

func agentPostCommitReminderHook(marker string) string {
	return "#!/bin/sh\n" +
		"# " + marker + "\n" +
		"cat <<'EOF'\n" +
		"\n" +
		"Post-commit agent reminder:\n" +
		"- Push this commit to the run branch and inspect the draft PR CI checks.\n" +
		"- Treat CI as part of the implementation contract, not optional evidence.\n" +
		"- Resolve merge conflicts against the current base branch before handoff.\n" +
		"- Do not hand off broken, pending, or intentionally failing CI unless the user explicitly accepted that state.\n" +
		"\n" +
		"EOF\n" +
		"exit 0\n"
}

func agentStepBaseEnv(base []string) []string {
	blocked := map[string]bool{
		"GLIMMUNG_ATTEMPT_TOKEN":                true,
		"GLIMMUNG_EVENTS_URL":                   true,
		"GLIMMUNG_STATUS_URL":                   true,
		"GLIMMUNG_COMPLETED_URL":                true,
		"GLIMMUNG_GITHUB_TOKEN_URL":             true,
		"GLIMMUNG_GITHUB_AGENT_TOKEN_URL":       true,
		"GLIMMUNG_GITHUB_PUSH_POLICY_TOKEN":     true,
		"GLIMMUNG_GITHUB_PUSH_POLICY_TOKEN_URL": true,
		"GLIMMUNG_PR_TOUCHPOINT_URL":            true,
		"GLIMMUNG_PR_MERGE_URL":                 true,
		"GLIMMUNG_SSH_CERT_URL":                 true,
		"GLIMMUNG_TAILSCALE_AUTHKEY_URL":        true,
		"GITHUB_TOKEN":                          true,
		"GH_TOKEN":                              true,
	}
	filtered := make([]string, 0, len(base))
	for _, row := range base {
		key, _, ok := strings.Cut(row, "=")
		if !ok {
			continue
		}
		if blocked[key] {
			continue
		}
		filtered = append(filtered, row)
	}
	return filtered
}

func (r *nativeRunner) agentPrompt(workdir string, step stepSpec, spec agentStepSpec, slot string, profile agentruntime.ResolvedProfile) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Glimmung agent task\n\n")
	fmt.Fprintf(&b, "- run: %s\n", strings.TrimSpace(os.Getenv("GLIMMUNG_RUN_REF")))
	fmt.Fprintf(&b, "- project: %s\n", strings.TrimSpace(os.Getenv("GLIMMUNG_PROJECT")))
	fmt.Fprintf(&b, "- workflow: %s\n", strings.TrimSpace(os.Getenv("GLIMMUNG_WORKFLOW")))
	fmt.Fprintf(&b, "- phase: %s\n", strings.TrimSpace(os.Getenv("GLIMMUNG_PHASE")))
	fmt.Fprintf(&b, "- job: %s\n", strings.TrimSpace(os.Getenv("GLIMMUNG_JOB_ID")))
	fmt.Fprintf(&b, "- step: %s\n", strings.TrimSpace(step.Slug))
	fmt.Fprintf(&b, "- agent slot: %s\n", slot)
	fmt.Fprintf(&b, "- agent profile: %s (%s %s)\n\n", profile.ProfileID, profile.Provider, profile.Model)
	if issueTitle := strings.TrimSpace(os.Getenv("GLIMMUNG_ISSUE_TITLE")); issueTitle != "" {
		fmt.Fprintf(&b, "## Issue\n\n%s\n\n", issueTitle)
	}
	if issueBody := strings.TrimSpace(os.Getenv("GLIMMUNG_ISSUE_BODY")); issueBody != "" {
		fmt.Fprintf(&b, "## Issue body\n\n%s\n\n", issueBody)
	}
	if feedback := strings.TrimSpace(os.Getenv("GLIMMUNG_FEEDBACK")); feedback != "" {
		fmt.Fprintf(&b, "## Human feedback\n\n%s\n\n", feedback)
	}
	if reqs := strings.TrimSpace(os.Getenv("GLIMMUNG_EVIDENCE_REQUIREMENTS_JSON")); reqs != "" {
		fmt.Fprintf(&b, "## Evidence requirements\n\n```json\n%s\n```\n\n", reqs)
	}
	if caseJSON := strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_JSON"]); caseJSON != "" {
		fmt.Fprintf(&b, "## Dynamic test case\n\n")
		fmt.Fprintf(&b, "- case index: %s of %s\n", strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_INDEX"]), strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_COUNT"]))
		fmt.Fprintf(&b, "- case label: %s\n", strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_LABEL"]))
		fmt.Fprintf(&b, "- template step: %s\n\n", strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_TEMPLATE_STEP"]))
		fmt.Fprintf(&b, "```json\n%s\n```\n\n", caseJSON)
	}
	if file := strings.TrimSpace(spec.PromptFile); file != "" {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(workdir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read agent prompt_file %q: %w", file, err)
		}
		fmt.Fprintf(&b, "## Project agent instructions\n\n%s\n\n", strings.TrimSpace(string(data)))
	}
	if inline := strings.TrimSpace(spec.Prompt); inline != "" {
		fmt.Fprintf(&b, "## Step instructions\n\n%s\n\n", inline)
	}
	return b.String(), nil
}

func agentRunScript(profile agentruntime.ResolvedProfile, workdir, promptFile, completionFile string) (string, error) {
	switch strings.TrimSpace(profile.Provider) {
	case agentruntime.ProviderCodex:
		args := []string{
			"exec",
			"--cd", workdir,
			"--model", profile.Model,
			"--dangerously-bypass-approvals-and-sandbox",
			"--json",
			"--output-last-message", filepath.Join(os.TempDir(), "glimmung-codex-last-message.md"),
			"-",
		}
		if strings.TrimSpace(profile.ReasoningEffort) != "" {
			args = append([]string{"exec", "-c", "model_reasoning_effort=" + shellQuoteForTOML(profile.ReasoningEffort)}, args[1:]...)
		}
		return agentShellPreamble() + "\n" +
			"last_message=" + shellQuoteArg(filepath.Join(os.TempDir(), "glimmung-codex-last-message.md")) + "\n" +
			"cat " + shellQuoteArg(promptFile) + " | codex " + shellJoin(args) + "\n" +
			"if [ -s \"$last_message\" ] && command -v jq >/dev/null 2>&1; then jq -Rs '{summary_markdown:.}' \"$last_message\" > " + shellQuoteArg(completionFile) + "; fi\n", nil
	case agentruntime.ProviderClaude:
		args := []string{
			"--print",
			"--model", profile.Model,
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions",
		}
		return claudeShellPreamble(workdir) + "\n" +
			"cat " + shellQuoteArg(promptFile) + " | claude " + shellJoin(args) + "\n", nil
	default:
		return "", fmt.Errorf("unsupported agent provider %q", profile.Provider)
	}
}

func agentShellPreamble() string {
	return `set -Eeuo pipefail
mkdir -p "$HOME/.codex"
cat > "$HOME/.codex/auth.json" <<'EOF'
{
  "auth_mode": "chatgptAuthTokens",
  "tokens": {
    "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJlbWFpbCI6ImdsaW1tdW5nQGxvY2FsIiwiZXhwIjo0MTAyNDQ0ODAwLCJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9wbGFuX3R5cGUiOiJwcm8iLCJjaGF0Z3B0X3VzZXJfaWQiOiJtYW5hZ2VkLWJ5LWdsaW1tdW5nIiwiY2hhdGdwdF9hY2NvdW50X2lkIjoibWFuYWdlZC1ieS1nbGltbXVuZyJ9fQ.signature",
    "access_token": "managed-by-glimmung",
    "refresh_token": "",
    "account_id": "managed-by-glimmung"
  },
  "last_refresh": "2099-01-01T00:00:00Z"
}
EOF
chmod 600 "$HOME/.codex/auth.json"
cat > "$HOME/.codex/config.toml" <<'EOF'
cli_auth_credentials_store = "file"
EOF
git config --global user.name "glimmung-agent[bot]" || true
git config --global user.email "glimmung-agent@romaine.life" || true`
}

func claudeShellPreamble(workdir string) string {
	return agentShellPreamble() + `
mkdir -p "$HOME/.claude"
cat > "$HOME/.claude/.credentials.json" <<'EOF'
{
  "claudeAiOauth": {
    "accessToken": "managed-by-glimmung",
    "refreshToken": "managed-by-glimmung",
    "expiresAt": 9999999999000,
    "scopes": ["user:inference", "user:profile"],
    "subscriptionType": "max",
    "rateLimitTier": "max"
  }
}
EOF
chmod 600 "$HOME/.claude/.credentials.json"
cat > "$HOME/.claude/settings.json" <<'EOF'
{"theme":"dark","permissions":{"defaultMode":"bypassPermissions"},"skipDangerousModePermissionPrompt":true}
EOF
cat > "$HOME/.claude.json" <<EOF
{
  "hasCompletedOnboarding": true,
  "officialMarketplaceAutoInstallAttempted": true,
  "officialMarketplaceAutoInstalled": true,
  "projects": {
    ` + jsonString(workdir) + `: {
      "allowedTools": [],
      "hasTrustDialogAccepted": true,
      "projectOnboardingSeenCount": 1
    }
  }
}
EOF`
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuoteArg(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuoteArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellQuoteForTOML(value string) string {
	b, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(b)
}

func jsonString(value string) string {
	b, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(b)
}

func shellCommand(shell, script string) (string, []string) {
	switch strings.TrimSpace(shell) {
	case "", "bash":
		return "bash", []string{"-e", "-u", "-o", "pipefail", "-c", script}
	case "sh":
		return "sh", []string{"-e", "-u", "-c", script}
	default:
		fields := strings.Fields(shell)
		if len(fields) == 0 {
			return "bash", []string{"-e", "-u", "-o", "pipefail", "-c", script}
		}
		return fields[0], append(fields[1:], "-c", script)
	}
}

func (r *nativeRunner) streamLogs(ctx context.Context, wg *sync.WaitGroup, stepSlug, stream string, reader io.Reader) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerTokenBytes)
	suppressEvidenceTar := false
	suppressedEvidenceLines := 0
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case evidenceTarStartMarker:
			suppressEvidenceTar = true
			suppressedEvidenceLines = 0
			r.forwardLogLine(ctx, stepSlug, stream, evidenceTarStartMarker+" payload omitted from native runner logs")
			continue
		case evidenceTarEndMarker:
			if suppressEvidenceTar {
				r.forwardLogLine(ctx, stepSlug, stream, fmt.Sprintf("%s omitted %d payload lines", evidenceTarEndMarker, suppressedEvidenceLines))
				suppressEvidenceTar = false
				suppressedEvidenceLines = 0
				continue
			}
		}
		if suppressEvidenceTar {
			suppressedEvidenceLines++
			continue
		}
		r.forwardLogLine(ctx, stepSlug, stream, line)
	}
	if suppressEvidenceTar {
		r.forwardLogLine(ctx, stepSlug, stream, fmt.Sprintf("unterminated evidence tar payload omitted from native runner logs after %d lines", suppressedEvidenceLines))
	}
	if err := scanner.Err(); err != nil {
		msg := "log stream read failed: " + err.Error()
		_ = r.postEvent(ctx, "runner_warning", &stepSlug, msg, nil, map[string]any{
			"warning": "log_stream_read_failed",
			"stream":  stream,
		})
	}
}

func (r *nativeRunner) forwardLogLine(ctx context.Context, stepSlug, stream, line string) {
	if line == "" {
		line = " "
	}
	line = sanitizeForwardedLogLine(line)
	r.observeLogCost(line)
	r.observeInnerJobMarker(ctx, stepSlug, line)
	if stream == "stderr" {
		fmt.Fprintln(os.Stderr, line)
	} else {
		fmt.Println(line)
	}
	r.postLogEvent(ctx, stepSlug, stream, line)
}

func (r *nativeRunner) postLogEvent(ctx context.Context, stepSlug, stream, line string) {
	if r.cfg.EventsURL == "" {
		return
	}
	// Allocate the sequence number HERE, in caller order, before handing the
	// HTTP POST to a goroutine. Log lines are read from the step's pipe in
	// order, but goroutine scheduling is not FIFO — assigning seq inside the
	// goroutine let later lines claim earlier sequence numbers, and the
	// events API (which orders strictly by seq) then replayed multi-line
	// output scrambled. Seen on ambience#167 run 5.1 emit-case-03, where a
	// pretty-printed JSON verdict came back shuffled. Network arrival order
	// is irrelevant; seq is the order contract.
	seq := r.nextSeq()
	go func() {
		postCtx, cancel := context.WithTimeout(ctx, logEventPostTimeout)
		defer cancel()
		_ = r.postEventWithSeq(postCtx, seq, "log", &stepSlug, line, nil, map[string]any{"stream": stream})
	}()
}

func sanitizeForwardedLogLine(line string) string {
	line = strings.ToValidUTF8(line, "?")
	if len(line) <= maxForwardedLogBytes {
		return line
	}
	omitted := len(line) - maxForwardedLogBytes
	return line[:maxForwardedLogBytes] + fmt.Sprintf("... [truncated %d bytes]", omitted)
}

func (r *nativeRunner) publishOutputs(ctx context.Context, stepSlug string, outputs map[string]string) error {
	keys := make([]string, 0, len(outputs))
	for key := range outputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := r.outputs[key]; exists {
			return fmt.Errorf("phase output %q already set", key)
		}
		value := outputs[key]
		if err := r.postEvent(ctx, "phase_output_set", &stepSlug, "", nil, map[string]any{
			"key":         key,
			"value":       value,
			"source_step": stepSlug,
		}); err != nil {
			return err
		}
		r.outputs[key] = value
	}
	return nil
}

func (r *nativeRunner) prepareCheckouts(ctx context.Context) error {
	if r.cfg.Job.Checkout != nil {
		if err := r.checkout(ctx, *r.cfg.Job.Checkout); err != nil {
			return err
		}
	}
	for _, checkout := range r.cfg.Job.ExtraCheckouts {
		if err := r.checkout(ctx, checkout); err != nil {
			return err
		}
	}
	return nil
}

func (r *nativeRunner) checkout(ctx context.Context, checkout checkoutSpec) error {
	token, err := r.githubToken(ctx)
	if err != nil {
		return err
	}
	repo := strings.TrimSpace(checkout.Repo)
	if repo == "" {
		return errors.New("checkout repo required")
	}
	path := checkout.Path
	if path == "" {
		path = filepath.Join(r.cfg.Workspace, repoBaseName(repo))
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("checkout path already exists: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	url := "https://x-access-token:" + token.Token + "@github.com/" + repo + ".git"
	if err := runCapture(ctx, "", "git", "clone", url, path); err != nil {
		return scrubToken(err, token.Token)
	}
	if err := runCapture(ctx, path, "git", "remote", "set-url", "origin", "https://github.com/"+repo+".git"); err != nil {
		return err
	}
	if ref := strings.TrimSpace(checkout.Ref); ref != "" {
		if err := runCapture(ctx, path, "git", "checkout", ref); err != nil {
			return scrubToken(err, token.Token)
		}
	}
	return nil
}

func (r *nativeRunner) githubToken(ctx context.Context) (githubTokenResult, error) {
	if r.githubTokenCache != nil {
		return *r.githubTokenCache, nil
	}
	if r.cfg.GitHubTokenURL == "" {
		return githubTokenResult{}, errors.New("GLIMMUNG_GITHUB_TOKEN_URL required for checkout")
	}
	var result githubTokenResult
	if err := r.postJSON(ctx, r.cfg.GitHubTokenURL, map[string]any{}, &result); err != nil {
		return githubTokenResult{}, err
	}
	if result.Token == "" {
		return githubTokenResult{}, errors.New("GitHub token response did not include token")
	}
	r.githubTokenCache = &result
	return result, nil
}

func runCapture(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s failed: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func runOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func (r *nativeRunner) nextSeq() int {
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	return seq
}

func (r *nativeRunner) postEvent(ctx context.Context, event string, stepSlug *string, message string, exitCode *int, metadata map[string]any) error {
	if r.cfg.EventsURL == "" {
		return nil
	}
	return r.postEventWithSeq(ctx, r.nextSeq(), event, stepSlug, message, exitCode, metadata)
}

func (r *nativeRunner) postEventWithSeq(ctx context.Context, seq int, event string, stepSlug *string, message string, exitCode *int, metadata map[string]any) error {
	if r.cfg.EventsURL == "" {
		return nil
	}
	var messagePtr *string
	if message != "" {
		messagePtr = &message
	}
	req := nativeEventRequest{
		JobID:        r.cfg.JobID,
		Seq:          seq,
		Event:        event,
		AttemptIndex: r.cfg.AttemptIndex,
		StepSlug:     stepSlug,
		Message:      messagePtr,
		ExitCode:     exitCode,
		Metadata:     metadata,
	}
	return r.postJSON(ctx, r.cfg.EventsURL, req, nil)
}

func (r *nativeRunner) complete(ctx context.Context, conclusion, summary string) error {
	if r.cfg.CompletedURL == "" {
		return nil
	}
	verification := r.completion.Verification
	evidence := r.completion.Evidence
	if len(r.caseCompletions) > 0 {
		verification = aggregateDynamicCaseVerification(r.caseCompletions)
		evidence = aggregateDynamicCaseEvidence(r.caseCompletions, evidence)
	}
	req := completedRequest{
		JobID:        r.cfg.JobID,
		Conclusion:   conclusion,
		AttemptIndex: r.cfg.AttemptIndex,
		CostUSD:      r.observedCostUSD(),
		Outputs:      r.outputs,
	}
	if len(verification) > 0 {
		req.Verification = verification
	}
	if len(evidence) > 0 {
		req.Evidence = evidence
	}
	if strings.TrimSpace(r.completion.ScreenshotsMarkdown) != "" {
		req.ScreenshotsMarkdown = &r.completion.ScreenshotsMarkdown
	}
	if strings.TrimSpace(r.completion.SummaryMarkdown) != "" {
		req.SummaryMarkdown = &r.completion.SummaryMarkdown
	} else if strings.TrimSpace(summary) != "" {
		req.SummaryMarkdown = &summary
	}
	return r.postJSON(ctx, r.cfg.CompletedURL, req, nil)
}

func (r *nativeRunner) observeLogCost(line string) {
	cost, ok := agentcost.FromJSONLogLine(line)
	if !ok {
		return
	}
	r.mu.Lock()
	r.costUSD += cost
	r.mu.Unlock()
}

// observeInnerJobMarker inspects every streamed log line for the
// inner-Job registration sentinel emitted by phase scripts (see
// docs/inner-job-observation.md). When one is found, we forward it as
// an `inner_job_registered` event so glimmung records the child k8s
// Job alongside the outer one.
//
// Hot path: the prefix check is cheap; we only allocate when the
// marker is present. Malformed markers are surfaced as
// runner_warning events with the parse error so the operator knows
// the registration was attempted but rejected. The pipeline does not
// fail.
func (r *nativeRunner) observeInnerJobMarker(ctx context.Context, stepSlug, line string) {
	if !strings.HasPrefix(line, innerjob.Marker) {
		return
	}
	reg, err := innerjob.Parse(line)
	if err != nil {
		msg := "inner-job marker rejected: " + err.Error()
		_ = r.postEvent(ctx, "runner_warning", &stepSlug, msg, nil, map[string]any{
			"warning": "inner_job_marker_invalid",
		})
		return
	}
	_ = r.postEvent(ctx, "inner_job_registered", &stepSlug, "", nil, reg.Metadata())
}

func (r *nativeRunner) observedCostUSD() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.costUSD
}

func (r *nativeRunner) collectCompletionMetadata(path string, step stepSpec) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var metadata completionMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	if len(metadata.Verification) > 0 && strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_INDEX"]) != "" {
		index, _ := strconv.Atoi(strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_INDEX"]))
		r.caseCompletions = append(r.caseCompletions, dynamicCaseCompletion{
			Index:        index,
			Label:        strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_LABEL"]),
			Verification: metadata.Verification,
			Evidence:     metadata.Evidence,
		})
		return nil
	}
	if len(metadata.Verification) > 0 {
		r.completion.Verification = metadata.Verification
	}
	if len(metadata.Evidence) > 0 {
		r.completion.Evidence = metadata.Evidence
	}
	if strings.TrimSpace(metadata.ScreenshotsMarkdown) != "" {
		r.completion.ScreenshotsMarkdown = metadata.ScreenshotsMarkdown
	}
	if strings.TrimSpace(metadata.SummaryMarkdown) != "" {
		r.completion.SummaryMarkdown = metadata.SummaryMarkdown
	}
	return nil
}

func (r *nativeRunner) dynamicCaseFailed(index int) bool {
	_, _, failed := r.dynamicCaseFailure(index)
	return failed
}

func (r *nativeRunner) dynamicCaseFailureForStep(step stepSpec) (string, string, bool) {
	index, err := strconv.Atoi(strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_INDEX"]))
	if err != nil || index <= 0 {
		return "", "", false
	}
	return r.dynamicCaseFailure(index)
}

func (r *nativeRunner) dynamicCaseFailure(index int) (string, string, bool) {
	if index <= 0 {
		return "", "", false
	}
	for i := len(r.caseCompletions) - 1; i >= 0; i-- {
		tc := r.caseCompletions[i]
		if tc.Index != index {
			continue
		}
		status := strings.TrimSpace(fmt.Sprint(tc.Verification["status"]))
		if status != "fail" && status != "error" {
			return status, "", false
		}
		reasons := stringSliceFromAny(tc.Verification["reasons"])
		reason := ""
		for _, candidate := range reasons {
			if strings.TrimSpace(candidate) != "" {
				reason = strings.TrimSpace(candidate)
				break
			}
		}
		return status, reason, true
	}
	return "", "", false
}

func dynamicCaseStepLabel(step stepSpec) string {
	return firstNonEmpty(
		strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_LABEL"]),
		strings.TrimSpace(step.Env["GLIMMUNG_DYNAMIC_CASE_INDEX"]),
		"unknown",
	)
}

func aggregateDynamicCaseVerification(cases []dynamicCaseCompletion) map[string]any {
	status := "pass"
	reasons := []string{}
	evidenceRefs := []string{}
	evidence := []evidenceArtifact{}
	for _, tc := range cases {
		caseStatus := strings.TrimSpace(fmt.Sprint(tc.Verification["status"]))
		switch caseStatus {
		case "error":
			status = "error"
		case "fail":
			if status != "error" {
				status = "fail"
			}
		case "", "pass":
		default:
			if status == "pass" {
				status = "error"
			}
			reasons = append(reasons, fmt.Sprintf("%s: unknown verification status %q", dynamicCaseCompletionLabel(tc), caseStatus))
		}
		for _, reason := range stringSliceFromAny(tc.Verification["reasons"]) {
			if strings.TrimSpace(reason) == "" {
				continue
			}
			reasons = append(reasons, fmt.Sprintf("%s: %s", dynamicCaseCompletionLabel(tc), strings.TrimSpace(reason)))
		}
		evidenceRefs = appendMissingStrings(evidenceRefs, stringSliceFromAny(tc.Verification["evidence_refs"])...)
		evidence = appendEvidenceArtifacts(evidence, evidenceArtifactsFromAny(tc.Verification["evidence"])...)
		evidence = appendEvidenceArtifacts(evidence, tc.Evidence...)
	}
	out := map[string]any{
		"status": status,
		"cases":  dynamicCaseVerificationSummaries(cases),
	}
	if len(reasons) > 0 {
		out["reasons"] = reasons
	}
	if len(evidenceRefs) > 0 {
		out["evidence_refs"] = evidenceRefs
	}
	if len(evidence) > 0 {
		out["evidence"] = evidence
	}
	return out
}

func aggregateDynamicCaseEvidence(cases []dynamicCaseCompletion, existing []evidenceArtifact) []evidenceArtifact {
	out := appendEvidenceArtifacts(nil, existing...)
	for _, tc := range cases {
		out = appendEvidenceArtifacts(out, tc.Evidence...)
		out = appendEvidenceArtifacts(out, evidenceArtifactsFromAny(tc.Verification["evidence"])...)
	}
	return out
}

func dynamicCaseVerificationSummaries(cases []dynamicCaseCompletion) []map[string]any {
	out := make([]map[string]any, 0, len(cases))
	for _, tc := range cases {
		item := map[string]any{
			"index":  tc.Index,
			"label":  dynamicCaseCompletionLabel(tc),
			"status": strings.TrimSpace(fmt.Sprint(tc.Verification["status"])),
		}
		out = append(out, item)
	}
	return out
}

func dynamicCaseCompletionLabel(tc dynamicCaseCompletion) string {
	if strings.TrimSpace(tc.Label) != "" {
		return strings.TrimSpace(tc.Label)
	}
	if tc.Index > 0 {
		return fmt.Sprintf("case %02d", tc.Index)
	}
	return "case"
}

func stringSliceFromAny(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := strings.TrimSpace(fmt.Sprint(item)); s != "" && s != "<nil>" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func evidenceArtifactsFromAny(value any) []evidenceArtifact {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []evidenceArtifact
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func appendMissingStrings(out []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range out {
		seen[value] = true
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func appendEvidenceArtifacts(out []evidenceArtifact, values ...evidenceArtifact) []evidenceArtifact {
	seen := map[string]bool{}
	for _, item := range out {
		key := evidenceArtifactKey(item)
		if key != "" {
			seen[key] = true
		}
	}
	for _, item := range values {
		key := evidenceArtifactKey(item)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func evidenceArtifactKey(item evidenceArtifact) string {
	ref := firstNonEmpty(strings.TrimSpace(item.Ref), strings.TrimSpace(item.ArtifactPath), strings.TrimSpace(item.URL))
	if ref == "" {
		return ""
	}
	return strings.TrimSpace(item.Kind) + "\x00" + ref
}

func (r *nativeRunner) postJSON(ctx context.Context, url string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.AttemptToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.AttemptToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return err
		}
	}
	return nil
}

func parseOutputFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	outputs := map[string]string{}
	if bytes.HasPrefix(raw, []byte("{")) {
		if err := mergeOutputJSON(outputs, raw); err == nil {
			return outputs, nil
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") {
			if err := mergeOutputJSON(outputs, []byte(line)); err != nil {
				return nil, err
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("invalid output line %q", line)
		}
		if err := setOutput(outputs, strings.TrimSpace(key), value); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return outputs, nil
}

func mergeOutputJSON(outputs map[string]string, raw []byte) error {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	if keyRaw, ok := obj["key"]; ok {
		key := strings.TrimSpace(fmt.Sprint(keyRaw))
		value := stringifyOutputValue(obj["value"])
		return setOutput(outputs, key, value)
	}
	for key, value := range obj {
		if err := setOutput(outputs, strings.TrimSpace(key), stringifyOutputValue(value)); err != nil {
			return err
		}
	}
	return nil
}

func setOutput(outputs map[string]string, key, value string) error {
	if key == "" {
		return errors.New("output key required")
	}
	if _, exists := outputs[key]; exists {
		return fmt.Errorf("output %q declared more than once", key)
	}
	outputs[key] = value
	return nil
}

func stringifyOutputValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64, bool:
		return fmt.Sprint(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(raw)
	}
}

func mergedEnv(base []string, maps ...map[string]string) []string {
	values := map[string]string{}
	order := make([]string, 0, len(base))
	for _, row := range base {
		key, value, ok := strings.Cut(row, "=")
		if !ok {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for _, m := range maps {
		for key, value := range m {
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = value
		}
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+values[key])
	}
	return out
}

func mergeStringMaps(base map[string]string, overlays ...map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range base {
		out[key] = value
	}
	for _, overlay := range overlays {
		for key, value := range overlay {
			out[key] = value
		}
	}
	return out
}

func safeStepSlug(value string) string {
	slug := normalizeOutputKey(value)
	if slug == "" {
		return "dynamic-group"
	}
	return strings.ReplaceAll(slug, "_", "-")
}

func repoBaseName(repo string) string {
	repo = strings.TrimSuffix(repo, ".git")
	parts := strings.Split(repo, "/")
	return parts[len(parts)-1]
}

func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
