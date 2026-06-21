package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/agentcost"
	"github.com/romaine-life/glimmung/internal/domain/steperr"
)

func testRate() agentcost.Rate {
	return agentcost.Rate{
		CatalogRef:               "test-catalog",
		InputPerMillionUSD:       2,
		CachedInputPerMillionUSD: 0.2,
		OutputPerMillionUSD:      10,
	}
}

// usageLine is the exact stream-json shape agentcost prices (see
// internal/domain/agentcost/agentcost_test.go). 750k uncached input @ $2 +
// 250k cached @ $0.2 + 100k output @ $10 = $2.55.
const usageLine = `{"type":"turn.completed","usage":{"input_tokens":1000000,"cached_input_tokens":250000,"output_tokens":100000,"reasoning_output_tokens":10000}}`

// TestInvokeStreamsAndPricesUsage is the $0-bug regression guard: a fake agent
// emits a usage line, and Invoke must (a) forward it as an intact single line
// the runner's stream parser can price, and (b) price it itself.
func TestInvokeStreamsAndPricesUsage(t *testing.T) {
	var stdout bytes.Buffer
	out, lerr := Invoke(context.Background(), Spec{
		Name:    "bash",
		Args:    []string{"-c", "echo 'thinking...'; printf '%s\\n' '" + usageLine + "'; echo 'done'"},
		Pricing: testRate(),
		Stdout:  &stdout,
	})
	if lerr != nil {
		t.Fatalf("Invoke errored: %v", lerr)
	}
	if !out.Ran {
		t.Fatal("Outcome.Ran should be true")
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", out.ExitCode)
	}
	if len(out.Usage) != 1 {
		t.Fatalf("priced %d usage observations, want 1", len(out.Usage))
	}
	if out.CostUSD != 2.55 {
		t.Fatalf("CostUSD = %v, want 2.55", out.CostUSD)
	}

	// The forwarded stream must carry the usage line intact so the runner's
	// own agentcost.FromJSONLogLine prices it — this is the exact path that
	// regressed to $0 when lines were merged/split.
	var pricedFromStream float64
	var observedLines int
	for _, line := range strings.Split(stdout.String(), "\n") {
		obs, observed, err := agentcost.FromJSONLogLine(line, testRate())
		if err != nil {
			t.Fatalf("forwarded line failed pricing: %v (line=%q)", err, line)
		}
		if observed {
			observedLines++
			pricedFromStream += obs.CostUSD
		}
	}
	if observedLines != 1 {
		t.Fatalf("forwarded stream had %d priceable usage lines, want 1", observedLines)
	}
	if pricedFromStream != 2.55 {
		t.Fatalf("stream-priced cost = %v, want 2.55", pricedFromStream)
	}
	if out.LastStdoutLine != "done" {
		t.Fatalf("LastStdoutLine = %q, want \"done\"", out.LastStdoutLine)
	}
}

func TestInvokeNonZeroExitIsModelLayer(t *testing.T) {
	out, lerr := Invoke(context.Background(), Spec{
		Name: "bash",
		Args: []string{"-c", "echo boom >&2; exit 7"},
	})
	if lerr == nil {
		t.Fatal("expected a model-layer error on non-zero exit")
	}
	if lerr.Layer != steperr.LayerModel {
		t.Fatalf("error layer = %q, want model", lerr.Layer)
	}
	if !out.Ran {
		t.Fatal("a non-zero exit still RAN the model")
	}
	if out.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", out.ExitCode)
	}
}

func TestInvokeStartFailureIsModelLayer(t *testing.T) {
	_, lerr := Invoke(context.Background(), Spec{Name: "this-binary-does-not-exist-glimmung"})
	if lerr == nil {
		t.Fatal("expected a model-layer error when the agent cannot start")
	}
	if lerr.Layer != steperr.LayerModel {
		t.Fatalf("error layer = %q, want model", lerr.Layer)
	}
	if lerr.Code != "agent_start_failed" {
		t.Fatalf("error code = %q, want agent_start_failed", lerr.Code)
	}
}

func TestInvokeEmptyNameIsModelMisconfig(t *testing.T) {
	_, lerr := Invoke(context.Background(), Spec{})
	if lerr == nil || lerr.Layer != steperr.LayerModel {
		t.Fatalf("empty name should be a model-layer error, got %v", lerr)
	}
}
