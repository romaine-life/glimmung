import { agentProfileLabel, type AgentRuntimeProfile } from "./agentRuntime";

type AgentRuntimePolicyFieldsProps = {
  profiles: AgentRuntimeProfile[];
  defaultProfile: string;
  onDefaultProfileChange: (profile: string) => void;
  slotProfiles?: Record<string, string>;
  onSlotProfileChange?: (slot: string, profile: string) => void;
  slots?: string[];
  inheritedLabel?: string;
};

export function AgentRuntimePolicyFields({
  profiles,
  defaultProfile,
  onDefaultProfileChange,
  slotProfiles = {},
  onSlotProfileChange,
  slots = [],
  inheritedLabel = "project default",
}: AgentRuntimePolicyFieldsProps) {
  return (
    <>
      <label>
        <span>Agent runtime</span>
        <select value={defaultProfile} onChange={(e) => onDefaultProfileChange(e.target.value)}>
          <option value="">Inherit ({inheritedLabel})</option>
          {profiles.map((profile) => (
            <option key={profile.id} value={profile.id}>
              {profile.id} - {agentProfileLabel(profile)}
            </option>
          ))}
        </select>
      </label>
      {slots.map((slot) => (
        <label key={slot}>
          <span>{slot} agent</span>
          <select
            value={slotProfiles[slot] ?? ""}
            onChange={(e) => onSlotProfileChange?.(slot, e.target.value)}
          >
            <option value="">Inherit</option>
            {profiles.map((profile) => (
              <option key={profile.id} value={profile.id}>
                {profile.id} - {agentProfileLabel(profile)}
              </option>
            ))}
          </select>
        </label>
      ))}
    </>
  );
}
