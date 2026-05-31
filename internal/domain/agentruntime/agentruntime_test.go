package agentruntime

import "testing"

func TestResolveAppliesExplicitInheritanceAcrossGlobalProjectIssue(t *testing.T) {
	global := DefaultConfig()
	global.Policy.Slots = map[string]PolicyDecision{
		"verification": {Mode: ModeOverride, Profile: "claude-sonnet"},
	}
	project := Config{
		Profiles: map[string]Profile{
			"project-codex": {Provider: ProviderCodex, Model: "gpt-5.4", ReasoningEffort: "high"},
		},
		Policy: Policy{
			Default: PolicyDecision{Mode: ModeOverride, Profile: "project-codex"},
			Slots: map[string]PolicyDecision{
				"implementation": {Mode: ModeInherit},
			},
		},
	}
	issue := &Policy{
		Slots: map[string]PolicyDecision{
			"implementation": {Mode: ModeOverride, Profile: "codex-fast"},
		},
	}

	snapshot, err := Resolve(global, project, true, issue, []string{"implementation", "verification"}, "pcs_123")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if snapshot.ProjectConfigSchemaRef != "pcs_123" {
		t.Fatalf("schema ref=%q", snapshot.ProjectConfigSchemaRef)
	}
	if snapshot.Default.ProfileID != "project-codex" || snapshot.Default.Source != "project" {
		t.Fatalf("default=%#v, want project-codex from project", snapshot.Default)
	}
	if got := snapshot.Slots["implementation"]; got.ProfileID != "codex-fast" || got.Source != "issue" {
		t.Fatalf("implementation=%#v, want issue codex-fast", got)
	}
	if got := snapshot.Slots["verification"]; got.ProfileID != "claude-sonnet" || got.Source != "global" {
		t.Fatalf("verification=%#v, want global claude-sonnet", got)
	}
}

func TestResolveRejectsUndefinedOverride(t *testing.T) {
	global := DefaultConfig()
	issue := &Policy{Default: PolicyDecision{Mode: ModeOverride, Profile: "missing"}}
	if _, err := Resolve(global, Config{}, false, issue, nil, ""); err == nil {
		t.Fatal("Resolve succeeded, want undefined profile error")
	}
}

func TestConfigFromMetadataValidatesShape(t *testing.T) {
	_, _, err := ConfigFromMetadata(map[string]any{
		"agent_runtime": map[string]any{
			"profiles": map[string]any{
				"Bad Profile": map[string]any{"provider": "codex", "model": "gpt-5.5"},
			},
		},
	})
	if err == nil {
		t.Fatal("ConfigFromMetadata succeeded, want identifier validation error")
	}
}
