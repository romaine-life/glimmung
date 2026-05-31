export type AgentRuntimeProfile = {
  id: string;
  provider: string;
  model: string;
  reasoning_effort?: string;
  metadata?: Record<string, unknown>;
};

export type AgentRuntimePolicyDecision = {
  mode: "inherit" | "override";
  profile?: string;
};

export type AgentRuntimePolicy = {
  default?: AgentRuntimePolicyDecision;
  slots?: Record<string, AgentRuntimePolicyDecision>;
};

export type AgentRuntimeConfig = {
  profiles?: Record<string, AgentRuntimeProfile>;
  policy?: AgentRuntimePolicy;
};

export type AgentRuntimeResolvedProfile = {
  profile_id: string;
  provider: string;
  model: string;
  reasoning_effort?: string;
  source: string;
};

export type AgentRuntimeSnapshot = {
  project_config_schema_ref?: string;
  default?: AgentRuntimeResolvedProfile;
  slots?: Record<string, AgentRuntimeResolvedProfile>;
};

export const AGENT_RUNTIME_DEFAULT_SLOT = "default";

export function agentRuntimeConfigFromMetadata(metadata: Record<string, unknown> | undefined | null): AgentRuntimeConfig | null {
  return agentRuntimeConfigFromUnknown(metadata?.agent_runtime);
}

export function agentRuntimeConfigFromUnknown(raw: unknown): AgentRuntimeConfig | null {
  if (!isRecord(raw)) return null;
  return raw as AgentRuntimeConfig;
}

export function agentRuntimeProfiles(...configs: Array<AgentRuntimeConfig | null | undefined>): AgentRuntimeProfile[] {
  const merged = new Map<string, AgentRuntimeProfile>();
  for (const config of configs) {
    for (const [id, profile] of Object.entries(config?.profiles ?? {})) {
      merged.set(id, { ...profile, id: profile.id || id });
    }
  }
  return Array.from(merged.values()).sort((a, b) => a.id.localeCompare(b.id));
}

export function agentProfileLabel(profile: AgentRuntimeProfile | AgentRuntimeResolvedProfile | null | undefined): string {
  if (!profile) return "inherit";
  const provider = "provider" in profile ? profile.provider : "";
  const model = "model" in profile ? profile.model : "";
  const effort = "reasoning_effort" in profile ? profile.reasoning_effort : undefined;
  return [provider, model, effort].filter(Boolean).join(" / ");
}

export function agentPolicyFromMetadata(metadata: Record<string, unknown> | undefined | null): AgentRuntimePolicy | null {
  const raw = metadata?.agent;
  return isRecord(raw) ? raw as AgentRuntimePolicy : null;
}

export function defaultAgentProfile(policy: AgentRuntimePolicy | null | undefined): string {
  const decision = policy?.default;
  return decision?.mode === "override" ? decision.profile ?? "" : "";
}

export function slotAgentProfiles(policy: AgentRuntimePolicy | null | undefined, slots: string[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const slot of slots) {
    const decision = policy?.slots?.[slot];
    out[slot] = decision?.mode === "override" ? decision.profile ?? "" : "";
  }
  return out;
}

export function buildAgentPolicy(defaultProfile: string, slotProfiles: Record<string, string>): AgentRuntimePolicy {
  const slots: Record<string, AgentRuntimePolicyDecision> = {};
  for (const [slot, profile] of Object.entries(slotProfiles)) {
    if (!slot.trim() || slot === AGENT_RUNTIME_DEFAULT_SLOT) continue;
    slots[slot] = profile
      ? { mode: "override", profile }
      : { mode: "inherit" };
  }
  return {
    default: defaultProfile
      ? { mode: "override", profile: defaultProfile }
      : { mode: "inherit" },
    ...(Object.keys(slots).length > 0 ? { slots } : {}),
  };
}

export function agentSlotsFromWorkflow(workflow: unknown): string[] {
  if (!isRecord(workflow)) return [];
  const slots = new Set<string>();
  const phases = Array.isArray(workflow.phases) ? workflow.phases : [];
  for (const phase of phases) {
    if (!isRecord(phase)) continue;
    const jobs = Array.isArray(phase.jobs) ? phase.jobs : [];
    for (const job of jobs) {
      if (!isRecord(job)) continue;
      const steps = Array.isArray(job.steps) ? job.steps : [];
      for (const step of steps) {
        if (!isRecord(step) || step.type !== "agent") continue;
        const agent = isRecord(step.agent) ? step.agent : {};
        const slot = typeof agent.slot === "string" && agent.slot.trim()
          ? agent.slot.trim()
          : AGENT_RUNTIME_DEFAULT_SLOT;
        if (slot !== AGENT_RUNTIME_DEFAULT_SLOT) slots.add(slot);
      }
    }
  }
  return Array.from(slots).sort();
}

export function agentRuntimeSnapshotLabel(snapshot: AgentRuntimeSnapshot | null | undefined): string {
  if (!snapshot?.default) return "not captured";
  const profile = snapshot.default.profile_id ? `${snapshot.default.profile_id}: ` : "";
  return `${profile}${agentProfileLabel(snapshot.default)}`;
}

export function agentRuntimeFromMetadata(metadata: Record<string, unknown> | undefined | null): AgentRuntimeSnapshot | null {
  const raw = metadata?.agent_runtime;
  return isRecord(raw) ? raw as AgentRuntimeSnapshot : null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
