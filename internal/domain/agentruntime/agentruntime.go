package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/romaine-life/glimmung/internal/domain/agentcost"
)

const (
	ProviderCodex  = "codex"
	ProviderClaude = "claude"

	ModeInherit  = "inherit"
	ModeOverride = "override"

	DefaultSlot = "default"
)

type Profile struct {
	ID              string         `json:"id,omitempty"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Pricing         agentcost.Rate `json:"pricing"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type PolicyDecision struct {
	Mode    string `json:"mode"`
	Profile string `json:"profile,omitempty"`
}

type Policy struct {
	Default PolicyDecision            `json:"default"`
	Slots   map[string]PolicyDecision `json:"slots,omitempty"`
}

type Config struct {
	Profiles map[string]Profile `json:"profiles"`
	Policy   Policy             `json:"policy"`
}

type ResolvedProfile struct {
	ProfileID       string         `json:"profile_id"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Pricing         agentcost.Rate `json:"pricing"`
	Source          string         `json:"source"`
}

type Snapshot struct {
	ProjectConfigSchemaRef string                     `json:"project_config_schema_ref,omitempty"`
	Default                ResolvedProfile            `json:"default"`
	Slots                  map[string]ResolvedProfile `json:"slots,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Profiles: map[string]Profile{
			"codex-deep": {
				Provider:        ProviderCodex,
				Model:           "gpt-5.5",
				ReasoningEffort: "xhigh",
				Pricing: agentcost.Rate{
					CatalogRef:               "agent-pricing-2026-06-13/gpt-5.5",
					InputPerMillionUSD:       5.00,
					CachedInputPerMillionUSD: 0.50,
					OutputPerMillionUSD:      22.50,
				},
			},
			"codex-balanced": {
				Provider:        ProviderCodex,
				Model:           "gpt-5.4",
				ReasoningEffort: "high",
				Pricing: agentcost.Rate{
					CatalogRef:               "agent-pricing-2026-06-13/gpt-5.4",
					InputPerMillionUSD:       2.50,
					CachedInputPerMillionUSD: 0.25,
					OutputPerMillionUSD:      11.25,
				},
			},
			"codex-fast": {
				Provider:        ProviderCodex,
				Model:           "gpt-5.4-mini",
				ReasoningEffort: "medium",
				Pricing: agentcost.Rate{
					CatalogRef:               "agent-pricing-2026-06-13/gpt-5.4-mini",
					InputPerMillionUSD:       0.375,
					CachedInputPerMillionUSD: 0.0375,
					OutputPerMillionUSD:      2.25,
				},
			},
			"claude-opus": {
				Provider: ProviderClaude,
				Model:    "claude-opus-4-8",
				Pricing: agentcost.Rate{
					CatalogRef:               "agent-pricing-2026-06-13/claude-opus-4-8",
					InputPerMillionUSD:       5.00,
					CachedInputPerMillionUSD: 0.50,
					OutputPerMillionUSD:      25.00,
				},
			},
			"claude-sonnet": {
				Provider: ProviderClaude,
				Model:    "claude-sonnet-4-6",
				Pricing: agentcost.Rate{
					CatalogRef:               "agent-pricing-2026-06-13/claude-sonnet-4-6",
					InputPerMillionUSD:       3.00,
					CachedInputPerMillionUSD: 0.30,
					OutputPerMillionUSD:      15.00,
				},
			},
		},
		Policy: Policy{
			Default: PolicyDecision{Mode: ModeOverride, Profile: "codex-deep"},
		},
	}
}

func ParseConfigJSON(raw string) (Config, error) {
	if strings.TrimSpace(raw) == "" {
		cfg := DefaultConfig()
		return cfg, ValidateRootConfig(cfg)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, err
	}
	return cfg, ValidateRootConfig(cfg)
}

func ConfigFromMetadata(metadata map[string]any) (Config, bool, error) {
	if len(metadata) == 0 {
		return Config{}, false, nil
	}
	raw, ok := metadata["agent_runtime"]
	if !ok {
		raw, ok = metadata["agentRuntime"]
	}
	if !ok || raw == nil {
		return Config{}, false, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Config{}, true, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, true, err
	}
	if err := ValidateExtensionConfig(cfg); err != nil {
		return Config{}, true, err
	}
	return cfg, true, nil
}

func ValidateRootConfig(cfg Config) error {
	if err := validateProfiles(cfg.Profiles); err != nil {
		return err
	}
	if len(cfg.Profiles) == 0 {
		return errors.New("agent_runtime.profiles must define at least one profile")
	}
	if strings.TrimSpace(cfg.Policy.Default.Mode) != ModeOverride {
		return errors.New("agent_runtime.policy.default must override a profile at the global level")
	}
	if _, ok := cfg.Profiles[strings.TrimSpace(cfg.Policy.Default.Profile)]; !ok {
		return fmt.Errorf("agent_runtime.policy.default profile %q is not defined", cfg.Policy.Default.Profile)
	}
	return validatePolicyShape(cfg.Policy)
}

func ValidateExtensionConfig(cfg Config) error {
	if err := validateProfiles(cfg.Profiles); err != nil {
		return err
	}
	return validatePolicyShape(cfg.Policy)
}

func ValidateIssuePolicy(policy *Policy) error {
	if policy == nil {
		return nil
	}
	return validatePolicyShape(*policy)
}

func ValidateSlot(slot string) error {
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return nil
	}
	if !validIdent(slot) {
		return fmt.Errorf("agent slot %q is not a stable identifier", slot)
	}
	return nil
}

func Resolve(global Config, project Config, projectHasConfig bool, issue *Policy, slots []string, projectConfigSchemaRef string) (Snapshot, error) {
	if err := ValidateRootConfig(global); err != nil {
		return Snapshot{}, fmt.Errorf("global agent runtime: %w", err)
	}
	if projectHasConfig {
		if err := ValidateExtensionConfig(project); err != nil {
			return Snapshot{}, fmt.Errorf("project agent runtime: %w", err)
		}
	}
	if err := ValidateIssuePolicy(issue); err != nil {
		return Snapshot{}, fmt.Errorf("issue agent runtime: %w", err)
	}
	profiles, err := mergedProfiles(global.Profiles, project.Profiles)
	if err != nil {
		return Snapshot{}, err
	}

	normalizedSlots := normalizeSlots(slots)
	currentDefault, err := resolveOverride(profiles, global.Policy.Default, "global")
	if err != nil {
		return Snapshot{}, err
	}
	slotValues := make(map[string]ResolvedProfile, len(normalizedSlots))
	slotBound := map[string]bool{}
	for _, slot := range normalizedSlots {
		slotValues[slot] = currentDefault
	}

	apply := func(policy Policy, source string) error {
		if d := normalizeDecision(policy.Default); d.Mode == ModeOverride {
			next, err := resolveOverride(profiles, d, source)
			if err != nil {
				return err
			}
			currentDefault = next
			for _, slot := range normalizedSlots {
				if !slotBound[slot] {
					slotValues[slot] = currentDefault
				}
			}
		}
		for slot, decision := range policy.Slots {
			slot = normalizeSlot(slot)
			if slot == "" {
				return errors.New("agent_runtime.policy.slots contains an empty slot")
			}
			normalized := normalizeDecision(decision)
			if normalized.Mode != ModeOverride {
				continue
			}
			next, err := resolveOverride(profiles, normalized, source)
			if err != nil {
				return fmt.Errorf("agent_runtime.policy.slots.%s: %w", slot, err)
			}
			slotValues[slot] = next
			slotBound[slot] = true
		}
		return nil
	}

	if err := apply(global.Policy, "global"); err != nil {
		return Snapshot{}, err
	}
	if projectHasConfig {
		if err := apply(project.Policy, "project"); err != nil {
			return Snapshot{}, err
		}
	}
	if issue != nil {
		if err := apply(*issue, "issue"); err != nil {
			return Snapshot{}, err
		}
	}

	return Snapshot{
		ProjectConfigSchemaRef: strings.TrimSpace(projectConfigSchemaRef),
		Default:                currentDefault,
		Slots:                  slotValues,
	}, nil
}

func (s Snapshot) ProfileForSlot(slot string) (ResolvedProfile, bool) {
	slot = normalizeSlot(slot)
	if slot == "" || slot == DefaultSlot {
		if s.Default.ProfileID == "" {
			return ResolvedProfile{}, false
		}
		return s.Default, true
	}
	if p, ok := s.Slots[slot]; ok && p.ProfileID != "" {
		return p, true
	}
	if s.Default.ProfileID == "" {
		return ResolvedProfile{}, false
	}
	return s.Default, true
}

func WorkflowSlots(stepSlots []string) []string {
	return normalizeSlots(stepSlots)
}

func normalizeSlots(slots []string) []string {
	seen := map[string]bool{DefaultSlot: true}
	out := []string{DefaultSlot}
	for _, slot := range slots {
		slot = normalizeSlot(slot)
		if slot == "" {
			slot = DefaultSlot
		}
		if seen[slot] {
			continue
		}
		seen[slot] = true
		out = append(out, slot)
	}
	sort.Strings(out[1:])
	return out
}

func normalizeSlot(slot string) string {
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return ""
	}
	return slot
}

func normalizeDecision(decision PolicyDecision) PolicyDecision {
	decision.Mode = strings.TrimSpace(decision.Mode)
	decision.Profile = strings.TrimSpace(decision.Profile)
	if decision.Mode == "" {
		decision.Mode = ModeInherit
	}
	return decision
}

func resolveOverride(profiles map[string]Profile, decision PolicyDecision, source string) (ResolvedProfile, error) {
	decision = normalizeDecision(decision)
	if decision.Mode != ModeOverride {
		return ResolvedProfile{}, fmt.Errorf("decision mode %q is not override", decision.Mode)
	}
	profile, ok := profiles[decision.Profile]
	if !ok {
		return ResolvedProfile{}, fmt.Errorf("profile %q is not defined", decision.Profile)
	}
	return ResolvedProfile{
		ProfileID:       decision.Profile,
		Provider:        profile.Provider,
		Model:           profile.Model,
		ReasoningEffort: profile.ReasoningEffort,
		Pricing:         profile.Pricing,
		Source:          source,
	}, nil
}

func mergedProfiles(global, project map[string]Profile) (map[string]Profile, error) {
	out := make(map[string]Profile, len(global)+len(project))
	for id, profile := range global {
		out[id] = profile
	}
	for id, profile := range project {
		if _, exists := out[id]; exists {
			return nil, fmt.Errorf("project agent_runtime profile %q duplicates a global profile; use a project-specific profile id", id)
		}
		out[id] = profile
	}
	return out, nil
}

func validateProfiles(profiles map[string]Profile) error {
	for id, profile := range profiles {
		if !validIdent(id) {
			return fmt.Errorf("agent_runtime profile id %q is not a stable identifier", id)
		}
		if strings.TrimSpace(profile.ID) != "" && strings.TrimSpace(profile.ID) != id {
			return fmt.Errorf("agent_runtime profile %q has mismatched id %q", id, profile.ID)
		}
		provider := strings.TrimSpace(profile.Provider)
		switch provider {
		case ProviderCodex, ProviderClaude:
		default:
			return fmt.Errorf("agent_runtime profile %q provider %q is not one of [codex, claude]", id, provider)
		}
		if strings.TrimSpace(profile.Model) == "" {
			return fmt.Errorf("agent_runtime profile %q model is required", id)
		}
		if err := agentcost.ValidateRate(profile.Pricing); err != nil {
			return fmt.Errorf("agent_runtime profile %q pricing: %w", id, err)
		}
	}
	return nil
}

func validatePolicyShape(policy Policy) error {
	if err := validateDecisionShape("default", policy.Default); err != nil {
		return err
	}
	for slot, decision := range policy.Slots {
		if !validIdent(slot) {
			return fmt.Errorf("agent_runtime policy slot %q is not a stable identifier", slot)
		}
		if err := validateDecisionShape("slot "+slot, decision); err != nil {
			return err
		}
	}
	return nil
}

func validateDecisionShape(label string, decision PolicyDecision) error {
	decision = normalizeDecision(decision)
	switch decision.Mode {
	case ModeInherit:
		if decision.Profile != "" {
			return fmt.Errorf("agent_runtime policy %s cannot set profile when mode=inherit", label)
		}
	case ModeOverride:
		if !validIdent(decision.Profile) {
			return fmt.Errorf("agent_runtime policy %s override profile %q is not a stable identifier", label, decision.Profile)
		}
	default:
		return fmt.Errorf("agent_runtime policy %s mode %q is not one of [inherit, override]", label, decision.Mode)
	}
	return nil
}

func validIdent(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for i, r := range value {
		if unicode.IsLower(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		if i == 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}
