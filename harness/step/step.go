// Package step is the spine of the Glimmung run-harness SDK. It holds
// Glimmung's run contract as Go types so an app physically cannot emit an
// untyped error, skip the verdict, or mislabel a harness crash as a model
// failure.
//
// A producing app registers one Handler per workflow step slug and calls
// Main(registry) from its binary's main(). Main reads GLIMMUNG_STEP_SLUG,
// builds a typed Context once from the GLIMMUNG_* environment, runs the
// handler under panic recovery, translates the outcome into the runner's wire
// shapes (GLIMMUNG_OUTPUT_FILE / GLIMMUNG_COMPLETION_FILE) and an honest exit
// code, and calls os.Exit itself.
//
// This package is the ONLY sanctioned step-producer surface. The migration
// guard test (step_producer_surface_test.go) asserts that, so later app slices
// cannot quietly reintroduce a hand-rolled lib.sh fork.
package step

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// Handler is one workflow step. Slug returns the step slug it serves
// (matching the runner's GLIMMUNG_STEP_SLUG); Run executes it against the typed
// Context. A nil error is success; a *LayeredError attributes a typed failure;
// a non-LayeredError is coerced to a harness-layer failure; calling
// ctx.Abort(reason) and returning its error requests a fail-closed abort.
type Handler interface {
	Slug() string
	Run(*Context) (Result, error)
}

// Result is the typed success outcome of a handler. Outputs are a convenience
// set of phase outputs merged into GLIMMUNG_OUTPUT_FILE after a successful Run
// (in addition to anything emitted via ctx.EmitOutput during the run);
// SummaryMarkdown, when set, overrides any ctx.SetSummaryMarkdown summary.
type Result struct {
	Outputs         map[string]string
	SummaryMarkdown string
}

// HandlerFunc adapts a func to a Handler.
type HandlerFunc struct {
	StepSlug string
	Fn       func(*Context) (Result, error)
}

func (h HandlerFunc) Slug() string                   { return h.StepSlug }
func (h HandlerFunc) Run(c *Context) (Result, error) { return h.Fn(c) }

// Registry maps step slugs to their handlers.
type Registry struct {
	handlers map[string]Handler
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Register adds handlers, panicking on an empty or duplicate slug — a
// programming error that must surface at startup, not at dispatch time.
func (r *Registry) Register(handlers ...Handler) *Registry {
	for _, h := range handlers {
		slug := strings.TrimSpace(h.Slug())
		if slug == "" {
			panic("step: handler with empty slug")
		}
		if _, exists := r.handlers[slug]; exists {
			panic("step: duplicate handler for slug " + slug)
		}
		r.handlers[slug] = h
	}
	return r
}

// Handler returns the handler for slug.
func (r *Registry) Handler(slug string) (Handler, bool) {
	h, ok := r.handlers[strings.TrimSpace(slug)]
	return h, ok
}

// Slugs returns the registered slugs, sorted.
func (r *Registry) Slugs() []string {
	out := make([]string, 0, len(r.handlers))
	for slug := range r.handlers {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// Exit codes for the honest exit contract.
const (
	exitSuccess     = 0
	exitFailure     = 1
	exitUnknownSlug = 2
)

// Main is the process entrypoint for an SDK-driven step binary. It never
// returns: it dispatches the requested slug and calls os.Exit with the honest
// code. Use Run for a testable core that returns the outcome instead of
// exiting.
func Main(r *Registry) {
	os.Exit(Run(r, context.Background(), os.Environ(), os.Stderr))
}

// Run is the testable core of Main. It builds the Context from env, dispatches
// the GLIMMUNG_STEP_SLUG handler, and returns the exit code, writing any typed
// failure to GLIMMUNG_COMPLETION_FILE and a human line to stderr. env is a
// list of "K=V" strings (os.Environ shape).
func Run(r *Registry, ctx context.Context, env []string, stderr *os.File) int {
	values := envMap(env)
	slug := strings.TrimSpace(values["GLIMMUNG_STEP_SLUG"])

	c, buildErr := newContext(ctx, values)
	if buildErr != nil {
		// A missing required wire path is a single typed harness failure, not
		// a mid-run crash.
		return finishFailure(c, values, stderr, buildErr)
	}

	if slug == "" {
		err := HarnessError("missing_step_slug", "GLIMMUNG_STEP_SLUG is not set", nil)
		return finishFailure(c, values, stderr, err)
	}

	handler, ok := r.Handler(slug)
	if !ok {
		// Unknown slug: exit 2 with a typed completion so the dispatch failure
		// is attributed, not a silent generic.
		err := HarnessError("unknown_step_slug", fmt.Sprintf("no handler registered for step slug %q (known: %s)", slug, strings.Join(r.Slugs(), ", ")), nil)
		writeCompletionFailure(c, err)
		fmt.Fprintln(stderr, err.Error())
		return exitUnknownSlug
	}

	result, runErr := runHandler(handler, c)

	// A fail-closed abort exits 0; the abort_reason output (already written by
	// ctx.Abort) is the routing signal.
	var abortErr *AbortError
	if asAbort(runErr, &abortErr) {
		return finishAbort(c, stderr, abortErr)
	}

	if runErr != nil {
		return finishFailure(c, values, stderr, runErr)
	}
	return finishSuccess(c, result, stderr)
}

// runHandler runs the handler under panic recovery. A panic becomes a typed
// harness-layer failure — the harness must never let a producer panic escape
// as an unattributed crash.
func runHandler(h Handler, c *Context) (result Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = HarnessError("panic", fmt.Sprintf("handler for step %q panicked: %v", c.StepSlug(), rec), nil)
		}
	}()
	return h.Run(c)
}

func finishSuccess(c *Context, result Result, stderr *os.File) int {
	if len(result.Outputs) > 0 {
		for _, key := range sortedKeys(result.Outputs) {
			if err := c.EmitOutput(key, result.Outputs[key]); err != nil {
				return finishFailure(c, c.env, stderr, err)
			}
		}
	}
	summary := strings.TrimSpace(result.SummaryMarkdown)
	if summary == "" {
		c.mu.Lock()
		summary = strings.TrimSpace(c.summary)
		c.mu.Unlock()
	}
	if summary != "" {
		if err := writeCompletion(c, completionDoc{SummaryMarkdown: summary}); err != nil {
			return finishFailure(c, c.env, stderr, err)
		}
	}
	return exitSuccess
}

func finishAbort(c *Context, stderr *os.File, abortErr *AbortError) int {
	reason := ""
	if abortErr != nil {
		reason = strings.TrimSpace(abortErr.Reason)
	}
	// abort_reason was written to the output file by ctx.Abort; surface a human
	// line and exit 0 so the runner routes to teardown-then-abort.
	if reason != "" {
		fmt.Fprintln(stderr, "abort: "+reason)
	} else {
		fmt.Fprintln(stderr, "abort requested")
	}
	return exitSuccess
}

func finishFailure(c *Context, env map[string]string, stderr *os.File, err error) int {
	le := asLayeredError(err)
	if c != nil {
		writeCompletionFailure(c, le)
	} else {
		// Context construction itself failed; write directly to the env path.
		writeCompletionFailureToPath(strings.TrimSpace(env["GLIMMUNG_COMPLETION_FILE"]), le)
	}
	fmt.Fprintln(stderr, le.Error())
	return exitFailure
}

// completionDoc is the GLIMMUNG_COMPLETION_FILE shape the SDK writes. It is a
// subset of the runner's completionMetadata: the SDK owns summary_markdown and
// the typed error block. The verification VERDICT is never written here — that
// is the glimmung-owned verification_finalize primitive's job, fed by
// artifacts/verification.json (see the harness/verification package).
type completionDoc struct {
	SummaryMarkdown string         `json:"summary_markdown,omitempty"`
	Error           *steperr.Block `json:"error,omitempty"`
}

func writeCompletionFailure(c *Context, le *LayeredError) {
	if c == nil {
		return
	}
	block := le.Block()
	doc := completionDoc{Error: &block}
	c.mu.Lock()
	if summary := strings.TrimSpace(c.summary); summary != "" {
		doc.SummaryMarkdown = summary
	}
	c.mu.Unlock()
	_ = writeCompletion(c, doc)
}

func writeCompletion(c *Context, doc completionDoc) error {
	return writeCompletionToPath(c.completionFile, doc)
}

func writeCompletionToPath(path string, doc completionDoc) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return HarnessError("completion_file", "encode completion", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return HarnessError("completion_file", "write completion file", err)
	}
	return nil
}

func writeCompletionFailureToPath(path string, le *LayeredError) {
	if strings.TrimSpace(path) == "" {
		return
	}
	block := le.Block()
	_ = writeCompletionToPath(path, completionDoc{Error: &block})
}

// newContext builds the typed Context, failing closed on a missing
// GLIMMUNG_OUTPUT_FILE or GLIMMUNG_COMPLETION_FILE — without those, the SDK
// cannot honor the wire contract, so it is a single typed harness error rather
// than a mid-run surprise.
func newContext(ctx context.Context, values map[string]string) (*Context, error) {
	outputFile := strings.TrimSpace(values["GLIMMUNG_OUTPUT_FILE"])
	completionFile := strings.TrimSpace(values["GLIMMUNG_COMPLETION_FILE"])
	c := &Context{
		ctx:            ctx,
		runID:          strings.TrimSpace(values["GLIMMUNG_RUN_ID"]),
		runRef:         strings.TrimSpace(values["GLIMMUNG_RUN_REF"]),
		project:        strings.TrimSpace(values["GLIMMUNG_PROJECT"]),
		workflow:       strings.TrimSpace(values["GLIMMUNG_WORKFLOW"]),
		phase:          strings.TrimSpace(values["GLIMMUNG_PHASE"]),
		jobID:          strings.TrimSpace(values["GLIMMUNG_JOB_ID"]),
		stepSlug:       strings.TrimSpace(values["GLIMMUNG_STEP_SLUG"]),
		issueRepo:      strings.TrimSpace(values["GLIMMUNG_ISSUE_REPO"]),
		issueNumber:    intFromEnv(values, "GLIMMUNG_ISSUE_NUMBER"),
		attemptIndex:   intFromEnv(values, "GLIMMUNG_ATTEMPT_INDEX"),
		workingDir:     strings.TrimSpace(values["GLIMMUNG_WORKING_DIR"]),
		outputFile:     outputFile,
		completionFile: completionFile,
		env:            values,
		outputs:        map[string]string{},
	}
	if outputFile == "" {
		return c, HarnessError("missing_output_file", "GLIMMUNG_OUTPUT_FILE is not set", nil)
	}
	if completionFile == "" {
		return c, HarnessError("missing_completion_file", "GLIMMUNG_COMPLETION_FILE is not set", nil)
	}
	return c, nil
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, row := range env {
		key, value, ok := strings.Cut(row, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func asAbort(err error, target **AbortError) bool {
	if err == nil {
		return false
	}
	if ae, ok := err.(*AbortError); ok {
		*target = ae
		return true
	}
	return false
}
