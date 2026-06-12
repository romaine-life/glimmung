// Recycle-policy visibility + control for a workflow definition.
//
// Every workflow's verify loop is bounded by a recycle policy: a lane
// fires on a trigger (`verify_fail`, `pr_review_changes_requested`, ...),
// lands back at an earlier phase, and retries up to `max_attempts` times
// before the decision engine gives up. That attempt count is the guard
// rail operators most often want to read and scale ("give this loop one
// more try"), but it had no surface — it lived only inside the workflow
// registration. This panel surfaces every lane's count and, for admins,
// lets them scale it. Saving PATCHes /v1/workflows/{project}/{name},
// which mints a new immutable workflow schema and moves the logical
// pointer forward (in-flight runs keep the schema they started with).
//
// Operator control pins: a pinned lane is frozen — re-registration cannot
// move it (the server enforces the pinned value into any incoming payload)
// and the scale input here is disabled until an admin unpins it. Pins are
// the durable answer to "I set max_attempts=1 and a later re-registration
// silently put it back to 3".
//
// Kept structural so anything shaped like a workflow with phases + a pr
// recycle policy can render through the project workflow view.

import { useEffect, useMemo, useState } from "react";
import { authedFetch } from "./auth";
import { Pill } from "./ui/bits";

export type RecyclePolicyLike = {
  max_attempts: number;
  on: string[];
  lands_at: string;
};

export type ControlPinLike = {
  pinned_by?: string;
  pinned_at?: string;
  reason?: string;
};

export type RecyclePolicyPhase = {
  name: string;
  recycle_policy: RecyclePolicyLike | null;
};

export type RecyclePolicyWorkflow = {
  project: string;
  name: string;
  phases: RecyclePolicyPhase[];
  pr: { recycle_policy: RecyclePolicyLike | null };
  control_pins?: Record<string, ControlPinLike>;
};

// Sentinel target for the workflow-level PR reject lane. Matches the Go
// server's RecyclePatchTargetPR.
const PR_TARGET = "pr";
const MIN_ATTEMPTS = 1;
const MAX_ATTEMPTS = 20;

// Control-pin target for a recycle lane; matches the Go server's
// ControlPinTargetForRecyclePatch.
function pinTargetFor(target: string): string {
  return target === PR_TARGET ? "pr.recycle_policy" : `phases.${target}.recycle_policy`;
}

type Lane = {
  target: string; // phase name, or PR_TARGET
  label: string;
  trigger: string;
  landsAt: string;
  maxAttempts: number;
  pin: ControlPinLike | null;
};

function lanesFromWorkflow(workflow: RecyclePolicyWorkflow): Lane[] {
  const pins = workflow.control_pins ?? {};
  const lanes: Lane[] = [];
  for (const phase of workflow.phases) {
    const policy = phase.recycle_policy;
    if (!policy) continue;
    lanes.push({
      target: phase.name,
      label: phase.name,
      trigger: policy.on.join(" / ") || "—",
      landsAt: policy.lands_at,
      maxAttempts: policy.max_attempts,
      pin: pins[pinTargetFor(phase.name)] ?? null,
    });
  }
  const prPolicy = workflow.pr.recycle_policy;
  if (prPolicy) {
    lanes.push({
      target: PR_TARGET,
      label: "pr reject",
      trigger: prPolicy.on.join(" / ") || "—",
      landsAt: prPolicy.lands_at,
      maxAttempts: prPolicy.max_attempts,
      pin: pins[pinTargetFor(PR_TARGET)] ?? null,
    });
  }
  return lanes;
}

function draftsFromLanes(lanes: Lane[]): Record<string, string> {
  return Object.fromEntries(lanes.map((lane) => [lane.target, String(lane.maxAttempts)]));
}

function pinTitle(pin: ControlPinLike): string {
  const parts = ["pinned"];
  if (pin.pinned_by) parts.push(`by ${pin.pinned_by}`);
  if (pin.reason) parts.push(`— ${pin.reason}`);
  return parts.join(" ");
}

export function RecyclePolicyPanel({
  workflow,
  signedIn,
  isAdmin,
  onSaved,
}: {
  workflow: RecyclePolicyWorkflow;
  signedIn: boolean;
  isAdmin: boolean;
  onSaved?: () => void;
}) {
  const lanes = useMemo(() => lanesFromWorkflow(workflow), [workflow]);
  const [drafts, setDrafts] = useState<Record<string, string>>(() => draftsFromLanes(lanes));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Per-lane pin flow state: "pin" shows the reason input, "unpin" the
  // two-step confirm. Only one lane's flow is open at a time.
  const [pinFlow, setPinFlow] = useState<{ target: string; mode: "pin" | "unpin"; reason: string } | null>(null);

  // Re-sync drafts to the latest workflow once a save settles or the
  // upstream definition changes underneath us (e.g. live SSE snapshot).
  useEffect(() => {
    if (!saving) setDrafts(draftsFromLanes(lanes));
  }, [lanes, saving]);

  const canEdit = signedIn && isAdmin;

  const changed = useMemo(
    () =>
      lanes.filter((lane) => {
        if (lane.pin) return false;
        const draft = drafts[lane.target];
        if (draft === undefined) return false;
        const parsed = Number.parseInt(draft, 10);
        return Number.isFinite(parsed) && parsed !== lane.maxAttempts;
      }),
    [lanes, drafts],
  );

  const save = async () => {
    if (!canEdit || changed.length === 0) return;
    const patches: { target: string; max_attempts: number }[] = [];
    for (const lane of changed) {
      const parsed = Number.parseInt(drafts[lane.target] ?? "", 10);
      if (!Number.isFinite(parsed) || parsed < MIN_ATTEMPTS || parsed > MAX_ATTEMPTS) {
        setError(`max attempts must be between ${MIN_ATTEMPTS} and ${MAX_ATTEMPTS}.`);
        return;
      }
      patches.push({ target: lane.target, max_attempts: parsed });
    }
    setSaving(true);
    setError(null);
    try {
      const response = await authedFetch(
        `/v1/workflows/${encodeURIComponent(workflow.project)}/${encodeURIComponent(workflow.name)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ recycle_max_attempts: patches }),
        },
      );
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
        return;
      }
      onSaved?.();
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  const applyPin = async (lane: Lane, mode: "pin" | "unpin", reason: string) => {
    setSaving(true);
    setError(null);
    try {
      const target = pinTargetFor(lane.target);
      const path = `/v1/workflows/${encodeURIComponent(workflow.project)}/${encodeURIComponent(workflow.name)}/control-pins/${encodeURIComponent(target)}`;
      const response = await authedFetch(path, {
        method: mode === "pin" ? "PUT" : "DELETE",
        ...(mode === "pin"
          ? {
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify(reason.trim() ? { reason: reason.trim() } : {}),
            }
          : {}),
      });
      if (!response.ok) {
        const text = await response.text().catch(() => "");
        setError(`${response.status} ${text || response.statusText}`);
        return;
      }
      setPinFlow(null);
      onSaved?.();
    } catch (err) {
      setError(String(err));
    } finally {
      setSaving(false);
    }
  };

  if (lanes.length === 0) {
    return (
      <section className="recycle-policy" aria-label="recycle policy">
        <div className="recycle-policy-head">
          <h3>recycle policy</h3>
        </div>
        <div className="dim mono">this workflow declares no recycle lanes.</div>
      </section>
    );
  }

  return (
    <section className="recycle-policy" aria-label="recycle policy">
      <div className="recycle-policy-head">
        <h3>recycle policy</h3>
        <span className="dim mono">max attempts before the verify loop gives up</span>
      </div>
      <table className="recycle-policy-table">
        <thead>
          <tr>
            <th>lane</th>
            <th>fires on</th>
            <th>lands at</th>
            <th>max attempts</th>
            {canEdit && <th aria-label="pin actions" />}
          </tr>
        </thead>
        <tbody>
          {lanes.map((lane) => {
            const draft = drafts[lane.target] ?? String(lane.maxAttempts);
            const inputId = `recycle-attempts-${lane.target}`;
            const flow = pinFlow?.target === lane.target ? pinFlow : null;
            return (
              <tr key={lane.target}>
                <td className="mono">{lane.label}</td>
                <td className="mono dim">{lane.trigger}</td>
                <td className="mono dim">{lane.landsAt}</td>
                <td>
                  {lane.pin ? (
                    <span title={pinTitle(lane.pin)}>
                      <span className="mono">{lane.maxAttempts}</span>{" "}
                      <Pill tone="info">pinned</Pill>
                    </span>
                  ) : canEdit ? (
                    <input
                      id={inputId}
                      aria-label={`${lane.label} max attempts`}
                      type="number"
                      min={MIN_ATTEMPTS}
                      max={MAX_ATTEMPTS}
                      value={draft}
                      disabled={saving}
                      onChange={(event) =>
                        setDrafts((current) => ({ ...current, [lane.target]: event.target.value }))
                      }
                    />
                  ) : (
                    <span className="mono">{lane.maxAttempts}</span>
                  )}
                </td>
                {canEdit && (
                  <td className="recycle-pin-actions">
                    {flow?.mode === "pin" ? (
                      <span className="mono">
                        <input
                          aria-label={`${lane.label} pin reason`}
                          type="text"
                          placeholder="reason (optional)"
                          value={flow.reason}
                          disabled={saving}
                          onChange={(event) =>
                            setPinFlow({ target: lane.target, mode: "pin", reason: event.target.value })
                          }
                        />{" "}
                        <button
                          type="button"
                          className="gb"
                          disabled={saving}
                          onClick={() => void applyPin(lane, "pin", flow.reason)}
                        >
                          pin
                        </button>{" "}
                        <button type="button" className="gb" disabled={saving} onClick={() => setPinFlow(null)}>
                          keep
                        </button>
                      </span>
                    ) : flow?.mode === "unpin" ? (
                      <span className="mono">
                        <button
                          type="button"
                          className="gb danger"
                          disabled={saving}
                          onClick={() => void applyPin(lane, "unpin", "")}
                        >
                          unpin?
                        </button>{" "}
                        <button type="button" className="gb" disabled={saving} onClick={() => setPinFlow(null)}>
                          keep
                        </button>
                      </span>
                    ) : lane.pin ? (
                      <button
                        type="button"
                        className="gb"
                        disabled={saving}
                        onClick={() => setPinFlow({ target: lane.target, mode: "unpin", reason: "" })}
                      >
                        unpin
                      </button>
                    ) : (
                      <button
                        type="button"
                        className="gb"
                        disabled={saving}
                        onClick={() => setPinFlow({ target: lane.target, mode: "pin", reason: "" })}
                      >
                        pin
                      </button>
                    )}
                  </td>
                )}
              </tr>
            );
          })}
        </tbody>
      </table>
      {canEdit ? (
        <div className="recycle-policy-actions">
          <button
            type="button"
            className="gb"
            disabled={saving || changed.length === 0}
            onClick={() => void save()}
          >
            {saving ? "saving..." : "apply"}
          </button>
          <span className="dim mono">
            {changed.length === 0 ? "no changes" : `${changed.length} change(s) pending`}
          </span>
        </div>
      ) : (
        <div className="dim mono">admin sign-in required to scale</div>
      )}
      {error && <div className="danger-text mono">{error}</div>}
    </section>
  );
}
