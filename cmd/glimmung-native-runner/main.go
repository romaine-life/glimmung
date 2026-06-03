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
	Job            jobSpec
	JobID          string
	AttemptIndex   *int
	EventsURL      string
	CompletedURL   string
	GitHubTokenURL string
	AttemptToken   string
	Workspace      string
	AgentRuntime   agentruntime.Snapshot
}

type nativeRunner struct {
	cfg              runnerConfig
	client           *http.Client
	seq              int
	outputs          map[string]string
	completion       completionMetadata
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

type githubTokenResult struct {
	Repo  string `json:"repo"`
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
		Job:            job,
		JobID:          jobID,
		AttemptIndex:   attemptIndex,
		EventsURL:      strings.TrimSpace(os.Getenv("GLIMMUNG_EVENTS_URL")),
		CompletedURL:   strings.TrimSpace(os.Getenv("GLIMMUNG_COMPLETED_URL")),
		GitHubTokenURL: strings.TrimSpace(os.Getenv("GLIMMUNG_GITHUB_TOKEN_URL")),
		AttemptToken:   token,
		Workspace:      firstNonEmpty(os.Getenv("GLIMMUNG_WORKSPACE"), defaultWorkspace),
		AgentRuntime:   runtime,
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
	for _, step := range r.cfg.Job.Steps {
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
	if err := r.postEvent(ctx, "step_started", &slug, "", nil, nil); err != nil {
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
	if completionErr := r.collectCompletionMetadata(completionFile); outputErr == nil && completionErr != nil {
		outputErr = completionErr
	}
	if execErr != nil {
		msg := fmt.Sprintf("step %s exited with code %d", slug, exitCode)
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exitCode, nil)
		return fmt.Errorf("%s: %w", msg, execErr)
	}
	if outputErr != nil {
		exit := 1
		msg := "step " + slug + " output error: " + outputErr.Error()
		_ = r.postEvent(ctx, "step_failed", &slug, msg, &exit, nil)
		return errors.New(msg)
	}
	if reason := r.requestedAbortReason(); reason != "" {
		msg := fmt.Sprintf("step %q requested run abort: %s", slug, reason)
		if err := r.postEvent(ctx, "step_aborted", &slug, msg, nil, map[string]any{
			"abort_reason": reason,
			"abort_scope":  "run_after_cleanup",
		}); err != nil {
			return err
		}
		return nil
	}
	if err := r.postEvent(ctx, "step_completed", &slug, "", &exitCode, nil); err != nil {
		return err
	}
	return nil
}

func (r *nativeRunner) executeStep(ctx context.Context, step stepSpec, outputFile, completionFile string) (int, error) {
	workdir := firstNonEmpty(step.WorkingDirectory, r.cfg.Job.WorkingDirectory, r.cfg.Workspace)
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return 1, err
	}
	name, args := shellCommand(firstNonEmpty(step.Shell, r.cfg.Job.Shell), step.Run)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workdir
	cmd.Env = mergedEnv(os.Environ(), r.cfg.Job.Env, step.Env, map[string]string{
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
	runStep := step
	runStep.Type = "run"
	runStep.Run, err = agentRunScript(profile, workdir, promptFile, completionFile)
	if err != nil {
		return 1, err
	}
	runStep.Shell = "bash"
	return r.executeStep(ctx, runStep, outputFile, completionFile)
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
	go func() {
		postCtx, cancel := context.WithTimeout(ctx, logEventPostTimeout)
		defer cancel()
		_ = r.postEvent(postCtx, "log", &stepSlug, line, nil, map[string]any{"stream": stream})
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
	repo := firstNonEmpty(checkout.Repo, token.Repo)
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

func (r *nativeRunner) postEvent(ctx context.Context, event string, stepSlug *string, message string, exitCode *int, metadata map[string]any) error {
	if r.cfg.EventsURL == "" {
		return nil
	}
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
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
	req := completedRequest{
		JobID:        r.cfg.JobID,
		Conclusion:   conclusion,
		AttemptIndex: r.cfg.AttemptIndex,
		CostUSD:      r.observedCostUSD(),
		Outputs:      r.outputs,
	}
	if len(r.completion.Verification) > 0 {
		req.Verification = r.completion.Verification
	}
	if len(r.completion.Evidence) > 0 {
		req.Evidence = r.completion.Evidence
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

func (r *nativeRunner) collectCompletionMetadata(path string) error {
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
