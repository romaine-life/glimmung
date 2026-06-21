package step

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/decision"
	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// stepEnv returns an os.Environ-shaped env for a step, with real temp file
// paths for the output and completion files.
func stepEnv(t *testing.T, slug string, extra map[string]string) (env []string, outputFile, completionFile string) {
	t.Helper()
	dir := t.TempDir()
	outputFile = filepath.Join(dir, "output.txt")
	completionFile = filepath.Join(dir, "completion.json")
	values := map[string]string{
		"GLIMMUNG_STEP_SLUG":       slug,
		"GLIMMUNG_OUTPUT_FILE":     outputFile,
		"GLIMMUNG_COMPLETION_FILE": completionFile,
		"GLIMMUNG_RUN_ID":          "run-1",
		"GLIMMUNG_RUN_REF":         "proj#1/runs/1",
		"GLIMMUNG_PROJECT":         "proj",
		"GLIMMUNG_WORKING_DIR":     dir,
	}
	for k, v := range extra {
		values[k] = v
	}
	for k, v := range values {
		env = append(env, k+"="+v)
	}
	return env, outputFile, completionFile
}

func readCompletion(t *testing.T, path string) completionDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read completion: %v", err)
	}
	var doc completionDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode completion %q: %v", string(raw), err)
	}
	return doc
}

func handler(slug string, fn func(*Context) (Result, error)) Handler {
	return HandlerFunc{StepSlug: slug, Fn: fn}
}

func TestRunUnknownSlugExitsTwoWithTypedCompletion(t *testing.T) {
	r := NewRegistry().Register(handler("known", func(*Context) (Result, error) { return Result{}, nil }))
	env, _, completionFile := stepEnv(t, "nope", nil)

	code := Run(r, context.Background(), env, os.Stderr)
	if code != exitUnknownSlug {
		t.Fatalf("exit code = %d, want %d", code, exitUnknownSlug)
	}
	doc := readCompletion(t, completionFile)
	if doc.Error == nil {
		t.Fatal("unknown slug must write a typed error completion")
	}
	if doc.Error.Layer != steperr.LayerHarness {
		t.Fatalf("error layer = %q, want harness", doc.Error.Layer)
	}
	if !strings.Contains(doc.Error.Message, "nope") {
		t.Fatalf("error message %q should name the unknown slug", doc.Error.Message)
	}
}

func TestRunAbortExitsZeroWithAbortReason(t *testing.T) {
	r := NewRegistry().Register(handler("abort-step", func(c *Context) (Result, error) {
		return Result{}, c.Abort("warm host asleep")
	}))
	env, outputFile, completionFile := stepEnv(t, "abort-step", nil)

	code := Run(r, context.Background(), env, os.Stderr)
	if code != exitSuccess {
		t.Fatalf("abort exit code = %d, want 0", code)
	}
	raw, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	want := decision.AbortReasonOutputKey + "=warm host asleep"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("output file %q must contain %q", string(raw), want)
	}
	// An abort must NOT write a typed error completion — it is exit 0.
	if _, err := os.Stat(completionFile); err == nil {
		doc := readCompletion(t, completionFile)
		if doc.Error != nil {
			t.Fatalf("abort wrote an error completion: %+v", doc.Error)
		}
	}
}

func TestContextInputFailsClosed(t *testing.T) {
	r := NewRegistry().Register(handler("needs-input", func(c *Context) (Result, error) {
		_, err := c.Input("validation_url")
		return Result{}, err
	}))
	env, _, completionFile := stepEnv(t, "needs-input", nil)

	code := Run(r, context.Background(), env, os.Stderr)
	if code != exitFailure {
		t.Fatalf("missing input exit = %d, want 1", code)
	}
	doc := readCompletion(t, completionFile)
	if doc.Error == nil || doc.Error.Layer != steperr.LayerHarness {
		t.Fatalf("missing input must be a harness-layer error, got %+v", doc.Error)
	}
	if doc.Error.Code != "missing_input" {
		t.Fatalf("error code = %q, want missing_input", doc.Error.Code)
	}
}

func TestContextInputPresentMirrorsLauncherEnvName(t *testing.T) {
	r := NewRegistry().Register(handler("reads-input", func(c *Context) (Result, error) {
		// The launcher emits GLIMMUNG_INPUT_GIT_REF for input "git_ref".
		v, err := c.Input("git_ref")
		if err != nil {
			return Result{}, err
		}
		if v != "abc123" {
			return Result{}, HarnessError("wrong", "got "+v, nil)
		}
		return Result{}, nil
	}))
	env, _, _ := stepEnv(t, "reads-input", map[string]string{"GLIMMUNG_INPUT_GIT_REF": "abc123"})
	if code := Run(r, context.Background(), env, os.Stderr); code != exitSuccess {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestLayerToCompletionTranslation(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantLayer string
		wantCause string
	}{
		{"harness", HarnessError("c", "boom", nil), steperr.LayerHarness, "harness_flake"},
		{"host", HostError("c", "host down", nil), steperr.LayerHost, "environment_config"},
		{"model", ModelError("c", "agent crashed", nil), steperr.LayerModel, "code_bug"},
		{"untyped-coerces-to-harness", os.ErrInvalid, steperr.LayerHarness, "harness_flake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry().Register(handler("s", func(*Context) (Result, error) { return Result{}, tc.err }))
			env, _, completionFile := stepEnv(t, "s", nil)
			code := Run(r, context.Background(), env, os.Stderr)
			if code != exitFailure {
				t.Fatalf("exit = %d, want 1", code)
			}
			doc := readCompletion(t, completionFile)
			if doc.Error == nil {
				t.Fatal("expected typed error completion")
			}
			if doc.Error.Layer != tc.wantLayer {
				t.Fatalf("layer = %q, want %q", doc.Error.Layer, tc.wantLayer)
			}
			if got := steperr.SuspectedCause(doc.Error.Layer); got != tc.wantCause {
				t.Fatalf("suspected cause = %q, want %q", got, tc.wantCause)
			}
		})
	}
}

func TestPanicBecomesHarnessFailure(t *testing.T) {
	r := NewRegistry().Register(handler("panics", func(*Context) (Result, error) { panic("kaboom") }))
	env, _, completionFile := stepEnv(t, "panics", nil)
	if code := Run(r, context.Background(), env, os.Stderr); code != exitFailure {
		t.Fatalf("exit = %d, want 1", code)
	}
	doc := readCompletion(t, completionFile)
	if doc.Error == nil || doc.Error.Layer != steperr.LayerHarness || doc.Error.Code != "panic" {
		t.Fatalf("panic must be a harness-layer panic error, got %+v", doc.Error)
	}
	if !strings.Contains(doc.Error.Message, "kaboom") {
		t.Fatalf("panic message %q should carry the panic value", doc.Error.Message)
	}
}

// TestEmitOutputParity proves the SDK's emitters produce output the runner's
// own parseOutputFile accepts and decodes to the same key/value map — the SDK
// and runner cannot drift on the output wire shape. parseOutputFileForTest
// mirrors cmd/glimmung-runner's parseOutputFile.
func TestEmitOutputParity(t *testing.T) {
	env, outputFile, _ := stepEnv(t, "emit", nil)
	r := NewRegistry().Register(handler("emit", func(c *Context) (Result, error) {
		if err := c.EmitOutput("simple", "value-1"); err != nil {
			return Result{}, err
		}
		if err := c.EmitOutput("multiline", "line1\nline2"); err != nil {
			return Result{}, err
		}
		if err := c.EmitJSONOutput("payload", map[string]any{"n": 5, "s": "x"}); err != nil {
			return Result{}, err
		}
		return Result{}, nil
	}))
	if code := Run(r, context.Background(), env, os.Stderr); code != exitSuccess {
		t.Fatalf("exit = %d, want 0", code)
	}
	outputs, err := parseOutputFileForTest(outputFile)
	if err != nil {
		t.Fatalf("runner-shaped parse failed: %v", err)
	}
	if outputs["simple"] != "value-1" {
		t.Fatalf("simple = %q", outputs["simple"])
	}
	if outputs["multiline"] != "line1\nline2" {
		t.Fatalf("multiline = %q, want the two-line value intact", outputs["multiline"])
	}
	// EmitJSONOutput stores the compact JSON as a string value (native parity).
	if outputs["payload"] != `{"n":5,"s":"x"}` {
		t.Fatalf("payload = %q", outputs["payload"])
	}
}

func TestEmitOutputDuplicateRejected(t *testing.T) {
	env, _, _ := stepEnv(t, "dup", nil)
	r := NewRegistry().Register(handler("dup", func(c *Context) (Result, error) {
		if err := c.EmitOutput("k", "a"); err != nil {
			return Result{}, err
		}
		return Result{}, c.EmitOutput("k", "b")
	}))
	if code := Run(r, context.Background(), env, os.Stderr); code != exitFailure {
		t.Fatalf("duplicate output should fail, exit = %d", code)
	}
}

func TestMissingOutputFileIsSingleHarnessError(t *testing.T) {
	r := NewRegistry().Register(handler("s", func(*Context) (Result, error) { return Result{}, nil }))
	// No GLIMMUNG_OUTPUT_FILE.
	dir := t.TempDir()
	completionFile := filepath.Join(dir, "c.json")
	env := []string{"GLIMMUNG_STEP_SLUG=s", "GLIMMUNG_COMPLETION_FILE=" + completionFile}
	if code := Run(r, context.Background(), env, os.Stderr); code != exitFailure {
		t.Fatalf("exit = %d, want 1", code)
	}
	doc := readCompletion(t, completionFile)
	if doc.Error == nil || doc.Error.Code != "missing_output_file" {
		t.Fatalf("want missing_output_file harness error, got %+v", doc.Error)
	}
}

// parseOutputFileForTest mirrors cmd/glimmung-runner/main.go's parseOutputFile
// closely enough to prove the SDK emitters round-trip. The authoritative,
// no-drift round-trip against the REAL parser lives in
// cmd/glimmung-runner/harness_contract_test.go.
func parseOutputFileForTest(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	outputs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				return nil, err
			}
			if keyRaw, ok := obj["key"]; ok {
				outputs[strings.TrimSpace(toStr(keyRaw))] = toStr(obj["value"])
				continue
			}
			for k, v := range obj {
				outputs[strings.TrimSpace(k)] = toStr(v)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, os.ErrInvalid
		}
		outputs[strings.TrimSpace(key)] = value
	}
	return outputs, nil
}

func toStr(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
