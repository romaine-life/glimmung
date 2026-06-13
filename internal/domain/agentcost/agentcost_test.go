package agentcost

import "testing"

func TestFromJSONLogLinePricesCodexUsage(t *testing.T) {
	rate := Rate{
		CatalogRef:               "test-catalog",
		InputPerMillionUSD:       2,
		CachedInputPerMillionUSD: 0.2,
		OutputPerMillionUSD:      10,
	}
	observation, ok, err := FromJSONLogLine(`{"type":"turn.completed","usage":{"input_tokens":1000000,"cached_input_tokens":250000,"output_tokens":100000,"reasoning_output_tokens":10000}}`, rate)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected usage observation")
	}
	if observation.CostUSD != 2.55 {
		t.Fatalf("cost=%v, want 2.55", observation.CostUSD)
	}
	if observation.Usage.InputTokens != 1000000 || observation.Usage.CachedInputTokens != 250000 || observation.Usage.OutputTokens != 100000 || observation.Usage.ReasoningOutputTokens != 10000 {
		t.Fatalf("usage=%#v", observation.Usage)
	}
	if observation.UncachedInputTokens != 750000 {
		t.Fatalf("uncached_input_tokens=%d", observation.UncachedInputTokens)
	}
}

func TestFromJSONLogLinePricesAnthropicCacheUsage(t *testing.T) {
	rate := Rate{
		CatalogRef:               "test-catalog",
		InputPerMillionUSD:       3,
		CachedInputPerMillionUSD: 0.3,
		OutputPerMillionUSD:      15,
	}
	observation, ok, err := FromJSONLogLine(`{"usage":{"input_tokens":100000,"cache_creation_input_tokens":10000,"cache_read_input_tokens":500000,"output_tokens":20000}}`, rate)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected usage observation")
	}
	if observation.CostUSD != 0.78 {
		t.Fatalf("cost=%v, want 0.78", observation.CostUSD)
	}
	if observation.Usage.CachedInputTokens != 500000 {
		t.Fatalf("cached_input_tokens=%d", observation.Usage.CachedInputTokens)
	}
	if observation.CacheWriteInputTokens != 10000 {
		t.Fatalf("cache_write_input_tokens=%d", observation.CacheWriteInputTokens)
	}
}

func TestFromJSONLogLineIgnoresLegacyCostAndNonUsage(t *testing.T) {
	rate := Rate{CatalogRef: "test", InputPerMillionUSD: 1, CachedInputPerMillionUSD: 0.1, OutputPerMillionUSD: 1}
	for _, line := range []string{
		`{"type":"result","total_cost_usd":1.2345}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":9}}}`,
		`not json`,
	} {
		if observation, ok, err := FromJSONLogLine(line, rate); err != nil || ok {
			t.Fatalf("line %q produced observation %#v ok=%v err=%v", line, observation, ok, err)
		}
	}
}

func TestFromJSONLogLineErrorsWhenUsageHasNoPricing(t *testing.T) {
	_, ok, err := FromJSONLogLine(`{"usage":{"input_tokens":1}}`, Rate{})
	if !ok {
		t.Fatal("expected usage observation")
	}
	if err == nil {
		t.Fatal("expected pricing error")
	}
}
