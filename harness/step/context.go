package step

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/romaine-life/glimmung/internal/domain/decision"
)

// nonEnvName mirrors internal/server/run_launcher.go's env-name normalization
// so Input("git_ref") reads exactly the GLIMMUNG_INPUT_GIT_REF the launcher
// emits. The SDK and the launcher must agree on this transform or fail-closed
// input resolution silently misses values.
var nonEnvName = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func envName(value string) string {
	return strings.Trim(strings.ToUpper(nonEnvName.ReplaceAllString(value, "_")), "_")
}

// Context is the typed view of one runner step's GLIMMUNG_* environment, built
// once by Main before the handler runs. Required wiring (the output and
// completion file paths) is validated at construction so a handler can never
// crash mid-run on a missing file path; that surfaces as a single typed
// harness-layer LayeredError instead.
type Context struct {
	ctx context.Context

	// Identity (typed accessors below read these).
	runID        string
	runRef       string
	project      string
	workflow     string
	phase        string
	jobID        string
	stepSlug     string
	issueRepo    string
	issueNumber  int
	attemptIndex int
	workingDir   string

	// Runner-managed file paths.
	outputFile     string
	completionFile string

	env map[string]string

	mu          sync.Mutex
	outputs     map[string]string
	summary     string
	aborted     bool
	abortReason string
}

// RunContext returns the underlying context.Context (cancellation, deadlines).
func (c *Context) RunContext() context.Context { return c.ctx }

// RunID returns the durable run id (GLIMMUNG_RUN_ID).
func (c *Context) RunID() string { return c.runID }

// RunRef returns the human run ref (GLIMMUNG_RUN_REF, e.g. "spirelens#177/runs/3").
func (c *Context) RunRef() string { return c.runRef }

// Project returns the project slug (GLIMMUNG_PROJECT).
func (c *Context) Project() string { return c.project }

// Workflow returns the workflow name (GLIMMUNG_WORKFLOW).
func (c *Context) Workflow() string { return c.workflow }

// Phase returns the phase name (GLIMMUNG_PHASE).
func (c *Context) Phase() string { return c.phase }

// JobID returns the job id (GLIMMUNG_JOB_ID).
func (c *Context) JobID() string { return c.jobID }

// StepSlug returns the dispatched step slug (GLIMMUNG_STEP_SLUG).
func (c *Context) StepSlug() string { return c.stepSlug }

// IssueRepo returns the issue repo (GLIMMUNG_ISSUE_REPO), empty when unset.
func (c *Context) IssueRepo() string { return c.issueRepo }

// IssueNumber returns the issue number (GLIMMUNG_ISSUE_NUMBER), 0 when unset.
func (c *Context) IssueNumber() int { return c.issueNumber }

// AttemptIndex returns the recycle attempt index (GLIMMUNG_ATTEMPT_INDEX).
func (c *Context) AttemptIndex() int { return c.attemptIndex }

// WorkingDir returns the run working directory (GLIMMUNG_WORKING_DIR).
func (c *Context) WorkingDir() string { return c.workingDir }

// OutputFilePath returns the path of GLIMMUNG_OUTPUT_FILE (phase outputs sink).
func (c *Context) OutputFilePath() string { return c.outputFile }

// CompletionFilePath returns the path of GLIMMUNG_COMPLETION_FILE.
func (c *Context) CompletionFilePath() string { return c.completionFile }

// Env returns a raw GLIMMUNG_* (or any) environment value by exact key.
func (c *Context) Env(key string) string { return c.env[key] }

// Input returns a declared phase input by its logical name, failing closed:
// a missing or empty GLIMMUNG_INPUT_<NAME> is a typed harness-layer
// LayeredError, never a silent empty string. This is the contract that makes a
// producer physically unable to proceed on a missing required value.
func (c *Context) Input(name string) (string, error) {
	return c.requireEnv("GLIMMUNG_INPUT_"+envName(name), fmt.Sprintf("required input %q", name))
}

// OptionalInput returns a declared phase input and whether it was present and
// non-empty. Use this only when absence is a legitimate, handled case.
func (c *Context) OptionalInput(name string) (string, bool) {
	v := strings.TrimSpace(c.env["GLIMMUNG_INPUT_"+envName(name)])
	return v, v != ""
}

// RunInput returns a durable run input by its logical name, failing closed on
// a missing or empty GLIMMUNG_RUN_INPUT_<NAME> exactly like Input.
func (c *Context) RunInput(name string) (string, error) {
	return c.requireEnv("GLIMMUNG_RUN_INPUT_"+envName(name), fmt.Sprintf("required run input %q", name))
}

// OptionalRunInput returns a durable run input and whether it was present.
func (c *Context) OptionalRunInput(name string) (string, bool) {
	v := strings.TrimSpace(c.env["GLIMMUNG_RUN_INPUT_"+envName(name)])
	return v, v != ""
}

func (c *Context) requireEnv(key, label string) (string, error) {
	value := strings.TrimSpace(c.env[key])
	if value == "" {
		return "", HarnessError("missing_input", fmt.Sprintf("%s is not set (env %s)", label, key), nil)
	}
	return value, nil
}

// EmitOutput records a phase output key=value. It mirrors the apps'
// native_emit_output (append a line to GLIMMUNG_OUTPUT_FILE that the runner's
// parseOutputFile reads) while being honest about values shell cannot carry:
// a value containing a newline is written as a single JSON object line, which
// the runner parses identically, instead of a multi-line key=value that the
// line parser would reject. A duplicate key is a typed harness error, matching
// the runner's "phase output already set" invariant.
func (c *Context) EmitOutput(key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return HarnessError("invalid_output", "output key required", nil)
	}
	c.mu.Lock()
	if _, exists := c.outputs[key]; exists {
		c.mu.Unlock()
		return HarnessError("duplicate_output", fmt.Sprintf("phase output %q already set", key), nil)
	}
	c.outputs[key] = value
	c.mu.Unlock()

	line := key + "=" + value + "\n"
	if strings.ContainsAny(value, "\n\r") || strings.HasPrefix(strings.TrimSpace(key), "{") {
		encoded, err := json.Marshal(map[string]string{"key": key, "value": value})
		if err != nil {
			return HarnessError("invalid_output", "encode output value", err)
		}
		line = string(encoded) + "\n"
	}
	return c.appendOutputFile(line)
}

// EmitJSONOutput records a phase output whose value is the compact JSON
// encoding of v, stored as a string value. It mirrors native_emit_json_output:
// the runner sees one output line whose value is the compact JSON text.
func (c *Context) EmitJSONOutput(key string, v any) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return HarnessError("invalid_output", "output key required", nil)
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return HarnessError("invalid_output", fmt.Sprintf("encode json output %q", key), err)
	}
	c.mu.Lock()
	if _, exists := c.outputs[key]; exists {
		c.mu.Unlock()
		return HarnessError("duplicate_output", fmt.Sprintf("phase output %q already set", key), nil)
	}
	c.outputs[key] = string(compact)
	c.mu.Unlock()

	encoded, err := json.Marshal(map[string]string{key: string(compact)})
	if err != nil {
		return HarnessError("invalid_output", "encode json output object", err)
	}
	return c.appendOutputFile(string(encoded) + "\n")
}

// Abort requests a fail-closed run abort with the operator-facing reason. It
// writes decision.AbortReasonOutputKey to the output file immediately — so the
// runner routes to teardown-then-abort even if the handler keeps going — and
// returns an *AbortError. A handler should return that error; Main translates
// it to a clean exit 0 with the abort_reason recorded. The key written is
// literally the runner's key, matching native_emit_abort.
func (c *Context) Abort(reason string) error {
	reason = strings.TrimSpace(reason)
	c.mu.Lock()
	c.aborted = true
	c.abortReason = reason
	already := c.outputs[decision.AbortReasonOutputKey] != ""
	if !already {
		c.outputs[decision.AbortReasonOutputKey] = reason
	}
	c.mu.Unlock()
	if !already {
		if err := c.appendOutputFile(decision.AbortReasonOutputKey + "=" + reason + "\n"); err != nil {
			return err
		}
	}
	return &AbortError{Reason: reason}
}

// SetSummaryMarkdown records the step's human summary (e.g. an agent's last
// message). Main writes it into GLIMMUNG_COMPLETION_FILE so it reaches the run
// report's why-channel.
func (c *Context) SetSummaryMarkdown(md string) {
	c.mu.Lock()
	c.summary = md
	c.mu.Unlock()
}

func (c *Context) appendOutputFile(line string) error {
	f, err := os.OpenFile(c.outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return HarnessError("output_file", "open output file", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return HarnessError("output_file", "write output file", err)
	}
	return nil
}

// AbortError is the sentinel returned by Context.Abort. Main recognizes it and
// exits 0 (the abort_reason output, not the exit code, is the routing signal).
type AbortError struct{ Reason string }

func (e *AbortError) Error() string {
	if e == nil || strings.TrimSpace(e.Reason) == "" {
		return "fail-closed abort requested"
	}
	return "fail-closed abort requested: " + e.Reason
}

func intFromEnv(env map[string]string, key string) int {
	if v := strings.TrimSpace(env[key]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}
