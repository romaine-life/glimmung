package agentcost

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// Rate is an explicit snapshot of the pricing catalog used for one agent
// profile. Prices are USD per million tokens.
type Rate struct {
	CatalogRef               string  `json:"catalog_ref"`
	InputPerMillionUSD       float64 `json:"input_per_million_usd"`
	CachedInputPerMillionUSD float64 `json:"cached_input_per_million_usd"`
	OutputPerMillionUSD      float64 `json:"output_per_million_usd"`
}

type Usage struct {
	InputTokens           int64 `json:"input_tokens,omitempty"`
	CachedInputTokens     int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens,omitempty"`
}

type Observation struct {
	Usage                 Usage   `json:"usage"`
	CostUSD               float64 `json:"cost_usd"`
	PricingCatalogRef     string  `json:"pricing_catalog_ref"`
	UncachedInputTokens   int64   `json:"uncached_input_tokens,omitempty"`
	CacheWriteInputTokens int64   `json:"cache_write_input_tokens,omitempty"`
}

func ValidateRate(rate Rate) error {
	if strings.TrimSpace(rate.CatalogRef) == "" {
		return errors.New("pricing catalog_ref is required")
	}
	if rate.InputPerMillionUSD <= 0 {
		return errors.New("pricing input_per_million_usd must be positive")
	}
	if rate.CachedInputPerMillionUSD < 0 {
		return errors.New("pricing cached_input_per_million_usd must not be negative")
	}
	if rate.OutputPerMillionUSD <= 0 {
		return errors.New("pricing output_per_million_usd must be positive")
	}
	return nil
}

// FromJSONLogLine extracts current provider usage events and prices them with
// the run's resolved profile rate. It intentionally ignores legacy
// total_cost_usd fields; live execution should derive cost from usage so a
// provider contract change cannot silently produce zero-cost runs.
func FromJSONLogLine(line string, rate Rate) (Observation, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return Observation{}, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return Observation{}, false, nil
	}
	rawUsage, ok := obj["usage"].(map[string]any)
	if !ok {
		return Observation{}, false, nil
	}
	usage, billableInput, cachedInput, cacheWriteInput, ok := parseUsage(rawUsage)
	if !ok {
		return Observation{}, false, nil
	}
	if err := ValidateRate(rate); err != nil {
		return Observation{}, true, err
	}
	cost := (float64(billableInput)/1_000_000.0)*rate.InputPerMillionUSD +
		(float64(cachedInput)/1_000_000.0)*rate.CachedInputPerMillionUSD +
		(float64(usage.OutputTokens)/1_000_000.0)*rate.OutputPerMillionUSD
	return Observation{
		Usage:                 usage,
		CostUSD:               roundCost(cost),
		PricingCatalogRef:     strings.TrimSpace(rate.CatalogRef),
		UncachedInputTokens:   billableInput,
		CacheWriteInputTokens: cacheWriteInput,
	}, true, nil
}

func parseUsage(raw map[string]any) (Usage, int64, int64, int64, bool) {
	input := intValue(raw["input_tokens"])
	codexCached := intValue(raw["cached_input_tokens"])
	anthropicCacheRead := intValue(raw["cache_read_input_tokens"])
	anthropicCacheWrite := intValue(raw["cache_creation_input_tokens"])
	output := intValue(raw["output_tokens"])
	reasoning := intValue(raw["reasoning_output_tokens"])

	if input == 0 && codexCached == 0 && anthropicCacheRead == 0 && anthropicCacheWrite == 0 && output == 0 && reasoning == 0 {
		return Usage{}, 0, 0, 0, false
	}

	cachedInput := codexCached
	billableInput := input - codexCached
	if billableInput < 0 {
		billableInput = 0
	}
	if anthropicCacheRead > 0 || anthropicCacheWrite > 0 {
		cachedInput = anthropicCacheRead
		billableInput = input + anthropicCacheWrite
	}

	return Usage{
		InputTokens:           input,
		CachedInputTokens:     cachedInput,
		OutputTokens:          output,
		ReasoningOutputTokens: reasoning,
	}, billableInput, cachedInput, anthropicCacheWrite, true
}

func intValue(raw any) int64 {
	switch value := raw.(type) {
	case json.Number:
		parsed, err := value.Int64()
		if err == nil && parsed > 0 {
			return parsed
		}
	case float64:
		if value > 0 {
			return int64(value)
		}
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return 0
		}
		dec := json.NewDecoder(strings.NewReader(value))
		dec.UseNumber()
		var parsed json.Number
		if err := dec.Decode(&parsed); err == nil {
			if i, err := parsed.Int64(); err == nil && i > 0 {
				return i
			}
		}
	}
	return 0
}

func roundCost(cost float64) float64 {
	if cost <= 0 {
		return 0
	}
	return math.Round(cost*1e12) / 1e12
}
