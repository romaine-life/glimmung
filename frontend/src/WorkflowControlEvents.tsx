// Attribution ledger for a workflow's control-plane writes: who registered,
// patched, pinned, unpinned, or deleted, when, and which control values
// moved. This is the durable answer to "max_attempts changed — who did
// that?" — the content-addressed schema rows can't say (a revert reuses an
// existing row), so the server appends one ledger event per write and this
// panel renders the recent tail. Read is public, like the other read
// surfaces.

import { useEffect, useState } from "react";

type ControlChange = {
  target: string;
  action: string;
  from?: unknown;
  to?: unknown;
};

type ControlEvent = {
  id: number;
  action: string;
  actor?: string;
  schema_ref?: string;
  detail?: {
    control_changes?: ControlChange[];
    pins_enforced?: string[];
    target?: string;
    reason?: string;
  };
  created_at: string;
};

function summarize(event: ControlEvent): string {
  const detail = event.detail ?? {};
  if (event.action === "pin" || event.action === "unpin") {
    const reason = detail.reason ? ` — ${detail.reason}` : "";
    return `${detail.target ?? "?"}${reason}`;
  }
  if (event.action === "delete") return "workflow deleted";
  const changes = detail.control_changes ?? [];
  if (changes.length === 0) return "no control changes";
  return changes
    .map((change) => {
      const fmt = (value: unknown) => {
        if (value === null || value === undefined) return "—";
        if (typeof value === "object" && value !== null && "max_attempts" in (value as Record<string, unknown>)) {
          return `×${(value as Record<string, unknown>).max_attempts}`;
        }
        if (typeof value === "object" && value !== null && "total" in (value as Record<string, unknown>)) {
          return `$${(value as Record<string, unknown>).total}`;
        }
        return JSON.stringify(value);
      };
      if (change.action === "pin_enforced") {
        return `${change.target} held at ${fmt(change.to)} (incoming ${fmt(change.from)} discarded)`;
      }
      return `${change.target} ${fmt(change.from)} → ${fmt(change.to)}`;
    })
    .join("; ");
}

export function WorkflowControlEvents({ project, name }: { project: string; name: string }) {
  const [events, setEvents] = useState<ControlEvent[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setEvents(null);
    setError(null);
    fetch(
      `/v1/workflows/${encodeURIComponent(project)}/${encodeURIComponent(name)}/control-events?limit=15`,
    )
      .then(async (response) => {
        if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
        const body = (await response.json()) as { events?: ControlEvent[] };
        if (!cancelled) setEvents(body.events ?? []);
      })
      .catch((err) => {
        if (!cancelled) setError(String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [project, name]);

  return (
    <section className="recycle-policy" aria-label="control history">
      <div className="recycle-policy-head">
        <h3>control history</h3>
        <span className="dim mono">who moved which control, attributed</span>
      </div>
      {error && <div className="danger-text mono">{error}</div>}
      {events === null && !error && <div className="dim mono">loading…</div>}
      {events !== null && events.length === 0 && (
        <div className="dim mono">no control events recorded yet.</div>
      )}
      {events !== null && events.length > 0 && (
        <table className="recycle-policy-table">
          <thead>
            <tr>
              <th>when</th>
              <th>actor</th>
              <th>action</th>
              <th>detail</th>
            </tr>
          </thead>
          <tbody>
            {events.map((event) => (
              <tr key={event.id}>
                <td className="mono dim">{new Date(event.created_at).toLocaleString()}</td>
                <td className="mono">{event.actor || "—"}</td>
                <td className="mono">{event.action}</td>
                <td className="mono dim">{summarize(event)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
