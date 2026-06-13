package runnercostrepair

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/romaine-life/glimmung/internal/domain/agentcost"
	"github.com/romaine-life/glimmung/internal/domain/agentruntime"
)

// Event is the small runner log event shape needed to repair persisted cost
// ledgers from already-durable runner result lines.
type Event struct {
	AttemptIndex int
	JobID        string
	Event        string
	StepSlug     string
	Message      string
}

type Patch struct {
	Path   string   `json:"path"`
	Before *float64 `json:"before,omitempty"`
	After  float64  `json:"after"`
}

type Result struct {
	Changed           bool    `json:"changed"`
	ObservedCostUSD   float64 `json:"observed_cost_usd"`
	CumulativeBefore  float64 `json:"cumulative_before"`
	CumulativeAfter   float64 `json:"cumulative_after"`
	AttemptPatchCount int     `json:"attempt_patch_count"`
	JobPatchCount     int     `json:"job_patch_count"`
	Patches           []Patch `json:"patches"`
}

// RepairRunPayload mutates a run payload so cost lives in the durable run
// ledger rather than being inferred by read-time UI projection code.
func RepairRunPayload(payload map[string]any, events []Event) (Result, error) {
	var result Result
	if payload == nil {
		return result, fmt.Errorf("run payload is nil")
	}
	runtime, err := runtimeSnapshotFromPayload(payload)
	if err != nil {
		return result, err
	}
	costByAttempt, costByAttemptJob, err := observedCosts(events, runtime)
	if err != nil {
		return result, err
	}
	for _, cost := range costByAttempt {
		result.ObservedCostUSD += cost
	}
	if len(costByAttempt) == 0 {
		before, _ := number(payload["cumulative_cost_usd"])
		result.CumulativeBefore = before
		result.CumulativeAfter = before
		return result, nil
	}

	attempts, ok := payload["attempts"].([]any)
	if !ok {
		return result, fmt.Errorf("payload attempts is missing or invalid")
	}
	for attemptPos, rawAttempt := range attempts {
		attempt, ok := rawAttempt.(map[string]any)
		if !ok {
			return result, fmt.Errorf("payload attempt %d is invalid", attemptPos)
		}
		attemptIndex := attemptPos
		if parsed, ok := intValue(attempt["attempt_index"]); ok {
			attemptIndex = parsed
		}
		observed := costByAttempt[attemptIndex]
		if observed > 0 {
			if patchCost(attempt, "cost_usd", observed, fmt.Sprintf("attempts[%d].cost_usd", attemptPos), &result) {
				result.AttemptPatchCount++
			}
			if verification, ok := attempt["verification"].(map[string]any); ok {
				patchCost(verification, "cost_usd", observed, fmt.Sprintf("attempts[%d].verification.cost_usd", attemptPos), &result)
			}
		}
		repairJobCompletions(attempt, attemptPos, costByAttemptJob[attemptIndex], &result)
	}

	before, _ := number(payload["cumulative_cost_usd"])
	after := sumAttemptCosts(attempts)
	result.CumulativeBefore = before
	result.CumulativeAfter = after
	if after > 0 && !sameNumber(before, after) {
		payload["cumulative_cost_usd"] = after
		result.Patches = append(result.Patches, Patch{
			Path:   "cumulative_cost_usd",
			Before: floatPointer(before),
			After:  after,
		})
		result.Changed = true
	}
	return result, nil
}

func observedCosts(events []Event, runtime agentruntime.Snapshot) (map[int]float64, map[int]map[string]float64, error) {
	costByAttempt := map[int]float64{}
	costByAttemptJob := map[int]map[string]float64{}
	for _, event := range events {
		if event.Event != "log" {
			continue
		}
		profile, ok := runtime.ProfileForSlot(agentruntime.DefaultSlot)
		if !ok {
			profile = runtime.Default
		}
		observation, observed, err := agentcost.FromJSONLogLine(event.Message, profile.Pricing)
		if !observed {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("price agent usage for attempt %d job %q step %q: %w", event.AttemptIndex, event.JobID, event.StepSlug, err)
		}
		cost := observation.CostUSD
		costByAttempt[event.AttemptIndex] += cost
		if costByAttemptJob[event.AttemptIndex] == nil {
			costByAttemptJob[event.AttemptIndex] = map[string]float64{}
		}
		costByAttemptJob[event.AttemptIndex][event.JobID] += cost
	}
	return costByAttempt, costByAttemptJob, nil
}

func runtimeSnapshotFromPayload(payload map[string]any) (agentruntime.Snapshot, error) {
	raw, ok := payload["agent_runtime"]
	if !ok || raw == nil {
		return agentruntime.Snapshot{}, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return agentruntime.Snapshot{}, fmt.Errorf("marshal agent_runtime snapshot: %w", err)
	}
	var runtime agentruntime.Snapshot
	if err := json.Unmarshal(data, &runtime); err != nil {
		return agentruntime.Snapshot{}, fmt.Errorf("decode agent_runtime snapshot: %w", err)
	}
	return runtime, nil
}

func repairJobCompletions(attempt map[string]any, attemptPos int, costs map[string]float64, result *Result) {
	if len(costs) == 0 {
		return
	}
	switch completions := attempt["job_completions"].(type) {
	case map[string]any:
		for key, rawCompletion := range completions {
			completion, ok := rawCompletion.(map[string]any)
			if !ok {
				continue
			}
			jobID := key
			if s, ok := completion["job_id"].(string); ok && s != "" {
				jobID = s
			}
			repairJobCompletion(completion, attemptPos, jobID, costs[jobID], result)
		}
	case []any:
		for completionPos, rawCompletion := range completions {
			completion, ok := rawCompletion.(map[string]any)
			if !ok {
				continue
			}
			jobID, _ := completion["job_id"].(string)
			pathJobID := jobID
			if pathJobID == "" {
				pathJobID = fmt.Sprintf("%d", completionPos)
			}
			repairJobCompletion(completion, attemptPos, pathJobID, costs[jobID], result)
		}
	}
}

func repairJobCompletion(completion map[string]any, attemptPos int, jobID string, observed float64, result *Result) {
	if observed <= 0 {
		return
	}
	path := fmt.Sprintf("attempts[%d].job_completions[%q].cost_usd", attemptPos, jobID)
	if patchCost(completion, "cost_usd", observed, path, result) {
		result.JobPatchCount++
	}
	if verification, ok := completion["verification"].(map[string]any); ok {
		patchCost(verification, "cost_usd", observed, fmt.Sprintf("attempts[%d].job_completions[%q].verification.cost_usd", attemptPos, jobID), result)
	}
}

func patchCost(target map[string]any, key string, value float64, path string, result *Result) bool {
	if value <= 0 {
		return false
	}
	before, ok := number(target[key])
	if ok && before > 0 {
		return false
	}
	target[key] = value
	result.Patches = append(result.Patches, Patch{
		Path:   path,
		Before: floatPointerIf(ok, before),
		After:  value,
	})
	result.Changed = true
	return true
}

func sumAttemptCosts(attempts []any) float64 {
	var total float64
	for _, rawAttempt := range attempts {
		attempt, ok := rawAttempt.(map[string]any)
		if !ok {
			continue
		}
		cost, ok := number(attempt["cost_usd"])
		if ok && cost > 0 {
			total += cost
		}
	}
	return total
}

func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		parsed, err := v.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func intValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		if math.Trunc(v) == v {
			return int(v), true
		}
	case json.Number:
		parsed, err := v.Int64()
		return int(parsed), err == nil
	}
	return 0, false
}

func sameNumber(a, b float64) bool {
	return math.Abs(a-b) < 0.000000001
}

func floatPointer(value float64) *float64 {
	return &value
}

func floatPointerIf(ok bool, value float64) *float64 {
	if !ok {
		return nil
	}
	return &value
}
