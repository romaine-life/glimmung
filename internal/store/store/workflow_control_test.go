package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/budget"
	"github.com/romaine-life/glimmung/internal/server"
	pgstore "github.com/romaine-life/glimmung/internal/store/pg"
)

func controlTestRegister() server.WorkflowRegister {
	return server.WorkflowRegister{
		Project: "ambience",
		Name:    "default",
		Budget:  budget.Config{Total: 25},
		PR: server.PrPrimitive{
			RecyclePolicy: &server.RecyclePolicy{MaxAttempts: 1, On: []string{"pr_review_changes_requested"}, LandsAt: "prepare"},
		},
		Phases: []server.PhaseSpec{
			{Name: "prepare"},
			{
				Name:          "llm-verify",
				Verify:        true,
				RecyclePolicy: &server.RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail", "verify_malformed"}, LandsAt: "prepare"},
			},
		},
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestEnforceControlPinsOverwritesPinnedRecyclePolicy(t *testing.T) {
	req := controlTestRegister()
	// Incoming registration tries to raise the verify budget to 3 — the
	// exact historical failure this feature exists to prevent.
	req.Phases[1].RecyclePolicy = &server.RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail", "verify_malformed"}, LandsAt: "prepare"}

	pinned := server.RecyclePolicy{MaxAttempts: 1, On: []string{"verify_fail", "verify_malformed"}, LandsAt: "prepare"}
	pins := map[string]server.ControlPin{
		"phases.llm-verify.recycle_policy": {Value: mustJSON(t, pinned), PinnedBy: "operator@romaine.life"},
	}

	enforced, changes, err := enforceControlPins(&req, pins)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if req.Phases[1].RecyclePolicy.MaxAttempts != 1 {
		t.Fatalf("pinned recycle policy not enforced: max_attempts=%d", req.Phases[1].RecyclePolicy.MaxAttempts)
	}
	if len(enforced) != 1 || enforced[0] != "phases.llm-verify.recycle_policy" {
		t.Fatalf("enforced=%v", enforced)
	}
	if len(changes) != 1 || changes[0].Action != "pin_enforced" {
		t.Fatalf("changes=%+v", changes)
	}
	if !strings.Contains(string(changes[0].From), `"max_attempts":3`) || !strings.Contains(string(changes[0].To), `"max_attempts":1`) {
		t.Fatalf("change payload from=%s to=%s", changes[0].From, changes[0].To)
	}
}

func TestEnforceControlPinsQuietWhenIncomingMatches(t *testing.T) {
	req := controlTestRegister()
	pins := map[string]server.ControlPin{
		"phases.llm-verify.recycle_policy": {Value: mustJSON(t, req.Phases[1].RecyclePolicy)},
	}
	enforced, changes, err := enforceControlPins(&req, pins)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if len(enforced) != 1 {
		t.Fatalf("enforced=%v", enforced)
	}
	if len(changes) != 0 {
		t.Fatalf("matching value must not report a change, got %+v", changes)
	}
}

func TestEnforceControlPinsBudgetAndPR(t *testing.T) {
	req := controlTestRegister()
	req.Budget.Total = 100
	req.PR.RecyclePolicy = &server.RecyclePolicy{MaxAttempts: 5, On: []string{"pr_review_changes_requested"}, LandsAt: "prepare"}

	pins := map[string]server.ControlPin{
		"budget":            {Value: mustJSON(t, budget.Config{Total: 25})},
		"pr.recycle_policy": {Value: mustJSON(t, server.RecyclePolicy{MaxAttempts: 1, On: []string{"pr_review_changes_requested"}, LandsAt: "prepare"})},
	}
	enforced, changes, err := enforceControlPins(&req, pins)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if req.Budget.Total != 25 {
		t.Fatalf("budget not enforced: %v", req.Budget.Total)
	}
	if req.PR.RecyclePolicy.MaxAttempts != 1 {
		t.Fatalf("pr policy not enforced: %d", req.PR.RecyclePolicy.MaxAttempts)
	}
	if len(enforced) != 2 || len(changes) != 2 {
		t.Fatalf("enforced=%v changes=%+v", enforced, changes)
	}
}

func TestEnforceControlPinsRejectsWhenPinnedPhaseMissing(t *testing.T) {
	req := controlTestRegister()
	req.Phases = req.Phases[:1] // drop llm-verify
	pins := map[string]server.ControlPin{
		"phases.llm-verify.recycle_policy": {Value: mustJSON(t, server.RecyclePolicy{MaxAttempts: 1})},
	}
	_, _, err := enforceControlPins(&req, pins)
	if err == nil {
		t.Fatal("expected rejection when pinned phase is missing")
	}
	if !strings.Contains(err.Error(), "unpin it first") {
		t.Fatalf("rejection must name the remediation, got: %v", err)
	}
}

func TestControlChangesDiffsBudgetPRAndPhases(t *testing.T) {
	prev := controlTestRegister()
	next := controlTestRegister()
	next.Budget.Total = 50
	next.Phases[1].RecyclePolicy = &server.RecyclePolicy{MaxAttempts: 3, On: []string{"verify_fail"}, LandsAt: "prepare"}
	next.Phases = append(next.Phases, server.PhaseSpec{
		Name:          "post-verify",
		RecyclePolicy: &server.RecyclePolicy{MaxAttempts: 2, On: []string{"verify_fail"}, LandsAt: "prepare"},
	})
	next.PR.RecyclePolicy = nil

	changes := controlChanges(&prev, next)
	got := map[string]string{}
	for _, change := range changes {
		got[change.Target] = change.Action
	}
	want := map[string]string{
		"budget":                            "changed",
		"pr.recycle_policy":                 "removed",
		"phases.llm-verify.recycle_policy":  "changed",
		"phases.post-verify.recycle_policy": "added",
	}
	for target, action := range want {
		if got[target] != action {
			t.Fatalf("target %q action=%q, want %q (all: %v)", target, got[target], action, got)
		}
	}
	if len(changes) != len(want) {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestControlChangesNilPrevIsFirstRegistration(t *testing.T) {
	next := controlTestRegister()
	if changes := controlChanges(nil, next); changes != nil {
		t.Fatalf("first registration must produce no diff, got %+v", changes)
	}
}

func TestControlValueAtRequiresExistingValue(t *testing.T) {
	reg := controlTestRegister()

	raw, err := controlValueAt(reg, "budget")
	if err != nil || !strings.Contains(string(raw), `"total":25`) {
		t.Fatalf("budget value=%s err=%v", raw, err)
	}
	if _, err := controlValueAt(reg, "phases.missing.recycle_policy"); err == nil {
		t.Fatal("missing phase must reject")
	}
	reg.Phases[1].RecyclePolicy = nil
	if _, err := controlValueAt(reg, "phases.llm-verify.recycle_policy"); err == nil {
		t.Fatal("nil policy must reject — a pin freezes what is, it does not invent configuration")
	}
}

func TestWorkflowFromRowAttachesControlPins(t *testing.T) {
	reg := controlTestRegister()
	doc := workflowDocFromRegister(reg, "2026-06-12T00:00:00Z")
	doc.Kind = "workflow"
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	pins := map[string]server.ControlPin{
		"budget": {Value: mustJSON(t, budget.Config{Total: 25}), PinnedBy: "operator@romaine.life", Reason: "spend ceiling"},
	}
	pinsPayload, err := json.Marshal(pins)
	if err != nil {
		t.Fatalf("marshal pins: %v", err)
	}

	workflow, err := workflowFromRow(pgstore.WorkflowRow{
		Project:     "ambience",
		Name:        "default",
		Payload:     payload,
		ControlPins: pinsPayload,
	})
	if err != nil {
		t.Fatalf("workflowFromRow: %v", err)
	}
	pin, ok := workflow.ControlPins["budget"]
	if !ok {
		t.Fatalf("control pins missing: %+v", workflow.ControlPins)
	}
	if pin.PinnedBy != "operator@romaine.life" || pin.Reason != "spend ceiling" {
		t.Fatalf("pin=%+v", pin)
	}

	// No pins → field omitted, not an empty map.
	bare, err := workflowFromRow(pgstore.WorkflowRow{Project: "ambience", Name: "default", Payload: payload})
	if err != nil {
		t.Fatalf("workflowFromRow bare: %v", err)
	}
	if bare.ControlPins != nil {
		t.Fatalf("expected nil pins, got %+v", bare.ControlPins)
	}
}

func TestPinRejectionNamesPinnerReasonAndRemediation(t *testing.T) {
	err := pinRejection("phases.llm-verify.recycle_policy", server.ControlPin{
		PinnedBy: "operator@romaine.life",
		Reason:   "systemic fails must not recycle",
	})
	for _, fragment := range []string{"pinned", "operator@romaine.life", "systemic fails", "unpin it first", "control-pins/phases.llm-verify.recycle_policy"} {
		if !strings.Contains(err.Message, fragment) {
			t.Fatalf("rejection %q missing %q", err.Message, fragment)
		}
	}
}
