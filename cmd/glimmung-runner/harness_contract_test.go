package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	harnessstep "github.com/romaine-life/glimmung/harness/step"
	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

// collectRunnerCallbacks spins up the events/completed sink the runner posts to.
func collectRunnerCallbacks(t *testing.T) (url string, events *[]runnerEventRequest, completion *completedRequest, client *http.Client) {
	t.Helper()
	var ev []runnerEventRequest
	var comp completedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			var e runnerEventRequest
			_ = json.NewDecoder(r.Body).Decode(&e)
			ev = append(ev, e)
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
		case "/completed":
			_ = json.NewDecoder(r.Body).Decode(&comp)
			_ = json.NewEncoder(w).Encode(map[string]any{"decision": "done"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, &ev, &comp, server.Client()
}

// TestRunnerPromotesTypedStepErrorOnFailure proves the §2 runner change: a
// producer step that writes a typed error block to GLIMMUNG_COMPLETION_FILE and
// exits non-zero rides that block onto the step_failed event metadata and into
// the /completed request.
func TestRunnerPromotesTypedStepErrorOnFailure(t *testing.T) {
	url, events, completion, client := collectRunnerCallbacks(t)
	workspace := t.TempDir()
	r := &runner{
		cfg: runnerConfig{
			JobID:        "job",
			EventsURL:    url + "/events",
			CompletedURL: url + "/completed",
			Workspace:    workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
				Steps: []stepSpec{{
					Slug: "prepare-host",
					Type: "run",
					Run:  `printf '{"error":{"layer":"host","code":"host_unreachable","message":"warm host asleep"}}' > "$GLIMMUNG_COMPLETION_FILE"; exit 1`,
				}},
			},
		},
		client:  client,
		outputs: map[string]string{},
	}

	if err := r.run(context.Background()); err == nil {
		t.Fatal("expected the step failure to propagate")
	}
	if completion.Conclusion != "failure" {
		t.Fatalf("conclusion=%q, want failure", completion.Conclusion)
	}
	if completion.Error == nil {
		t.Fatal("completed request must carry the typed step-error block")
	}
	if completion.Error.Layer != steperr.LayerHost || completion.Error.Message != "warm host asleep" {
		t.Fatalf("completed error = %+v", completion.Error)
	}
	stepFailed := findEvent(*events, "step_failed")
	if stepFailed == nil {
		t.Fatal("no step_failed event")
	}
	if stepFailed.Metadata["error_layer"] != "host" || stepFailed.Metadata["error_message"] != "warm host asleep" {
		t.Fatalf("step_failed metadata = %#v", stepFailed.Metadata)
	}
}

// TestRunnerFailureWithoutBlockIsUnchanged proves a producer failure with no
// typed block keeps the historical generic behavior: no error on the
// completion, no error_* metadata on step_failed.
func TestRunnerFailureWithoutBlockIsUnchanged(t *testing.T) {
	url, events, completion, client := collectRunnerCallbacks(t)
	workspace := t.TempDir()
	r := &runner{
		cfg: runnerConfig{
			JobID:        "job",
			EventsURL:    url + "/events",
			CompletedURL: url + "/completed",
			Workspace:    workspace,
			Job: jobSpec{
				WorkingDirectory: workspace,
				Shell:            "sh",
				Steps:            []stepSpec{{Slug: "build", Type: "run", Run: "exit 3"}},
			},
		},
		client:  client,
		outputs: map[string]string{},
	}
	if err := r.run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if completion.Error != nil {
		t.Fatalf("no-block failure must not carry an error, got %+v", completion.Error)
	}
	stepFailed := findEvent(*events, "step_failed")
	if stepFailed == nil {
		t.Fatal("no step_failed event")
	}
	if _, ok := stepFailed.Metadata["error_layer"]; ok {
		t.Fatalf("no-block step_failed must not carry error_layer: %#v", stepFailed.Metadata)
	}
	if stepFailed.Message == nil || *stepFailed.Message != "step build exited with code 3" {
		t.Fatalf("no-block step_failed message changed: %#v", stepFailed.Message)
	}
}

// TestSDKEmissionsRoundTripThroughRunnerParsers is the no-drift contract test:
// the harness/step SDK's output and completion emissions are read back by the
// runner's OWN parseOutputFile and collectCompletionMetadata, so the SDK and
// runner cannot diverge on the wire shapes.
func TestSDKEmissionsRoundTripThroughRunnerParsers(t *testing.T) {
	dir := t.TempDir()
	outputFile := filepath.Join(dir, "output.txt")
	completionFile := filepath.Join(dir, "completion.json")
	env := []string{
		"GLIMMUNG_STEP_SLUG=produce",
		"GLIMMUNG_OUTPUT_FILE=" + outputFile,
		"GLIMMUNG_COMPLETION_FILE=" + completionFile,
		"GLIMMUNG_WORKING_DIR=" + dir,
	}

	reg := harnessstep.NewRegistry().Register(harnessstep.HandlerFunc{
		StepSlug: "produce",
		Fn: func(c *harnessstep.Context) (harnessstep.Result, error) {
			if err := c.EmitOutput("preview_url", "https://example.test"); err != nil {
				return harnessstep.Result{}, err
			}
			if err := c.EmitJSONOutput("plan", map[string]any{"cases": 3}); err != nil {
				return harnessstep.Result{}, err
			}
			// A host-layer failure so the completion carries a typed error.
			return harnessstep.Result{}, harnessstep.HostError("host_unreachable", "warm host asleep", nil)
		},
	})
	if code := harnessstep.Run(reg, context.Background(), env, os.Stderr); code != 1 {
		t.Fatalf("SDK Run exit = %d, want 1", code)
	}

	// The runner's OWN parser reads the SDK's output file.
	outputs, err := parseOutputFile(outputFile)
	if err != nil {
		t.Fatalf("runner parseOutputFile rejected SDK output: %v", err)
	}
	if outputs["preview_url"] != "https://example.test" {
		t.Fatalf("preview_url = %q", outputs["preview_url"])
	}
	if outputs["plan"] != `{"cases":3}` {
		t.Fatalf("plan = %q", outputs["plan"])
	}

	// The runner's OWN completion collector reads the SDK's typed error block.
	r := &runner{cfg: runnerConfig{}, outputs: map[string]string{}}
	if err := r.collectCompletionMetadata(completionFile, stepSpec{Slug: "produce"}); err != nil {
		t.Fatalf("runner collectCompletionMetadata rejected SDK completion: %v", err)
	}
	if r.completion.Error == nil {
		t.Fatal("runner did not read the SDK's typed error block")
	}
	if r.completion.Error.Layer != steperr.LayerHost || r.completion.Error.Message == "" {
		t.Fatalf("runner read error = %+v", r.completion.Error)
	}
}

func findEvent(events []runnerEventRequest, name string) *runnerEventRequest {
	for i := range events {
		if events[i].Event == name {
			return &events[i]
		}
	}
	return nil
}
