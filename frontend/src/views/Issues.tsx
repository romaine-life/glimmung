import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Icon } from "../ui/Icon";
import { Pill } from "../ui/bits";
import { authedFetch } from "../auth";
import { attention, issueSummaryPath, useLayout, type IssueRow } from "./lib";

type IssueState = "open" | "closed" | "all";
type ActionStatus = { ref: string; kind: "dispatching" | "discarding" | "error" | "done"; message?: string } | null;

export function Issues() {
  const { signedIn } = useLayout();
  const [rows, setRows] = useState<IssueRow[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [state, setState] = useState<IssueState>("open");
  const [action, setAction] = useState<ActionStatus>(null);

  const refresh = async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      if (state !== "open") params.set("state", state);
      const url = params.size ? `/v1/issues?${params}` : "/v1/issues";
      const r = await fetch(url);
      if (!r.ok) throw new Error(`${url} -> ${r.status}`);
      setRows((await r.json()) as IssueRow[]);
    } catch (e) {
      setError(String(e));
      setRows(null);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state]);

  const dispatch = async (row: IssueRow) => {
    if (row.number == null) return;
    setAction({ ref: row.ref, kind: "dispatching" });
    try {
      const r = await authedFetch("/v1/runs/dispatch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ issue_number: row.number, project: row.project, workflow: row.workflow ?? undefined }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setAction({ ref: row.ref, kind: "done" });
      void refresh();
    } catch (e) {
      setAction({ ref: row.ref, kind: "error", message: String(e) });
    }
  };

  const discard = async (row: IssueRow) => {
    if (row.number == null) return;
    const reason = window.prompt("Discard reason", "");
    if (reason === null) return;
    setAction({ ref: row.ref, kind: "discarding" });
    try {
      const r = await authedFetch(`/v1/issues/by-number/${encodeURIComponent(row.project)}/${row.number}/discard`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ reason }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setAction(null);
      void refresh();
    } catch (e) {
      setAction({ ref: row.ref, kind: "error", message: String(e) });
    }
  };

  const inFlight = rows?.filter((r) => r.issue_lock_held).length ?? 0;

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Issues</h1>
          <div className="sub">Open issues across registered repos. Dispatch an agent run from any row.</div>
        </div>
      </div>

      <div className="toolbar">
        <div className="chip-filter" onClick={() => setState(state === "open" ? "closed" : state === "closed" ? "all" : "open")} role="button">
          <Icon name="issue" />State <span className="v">{state}</span><Icon name="chevdown" />
        </div>
        <button className="btn btn-ghost btn-sm" onClick={() => void refresh()} disabled={loading}>
          <Icon name="refresh" />{loading ? "refreshing…" : "refresh"}
        </button>
        <div className="spacer" />
        <span className="filter-note">{rows ? `${rows.length} ${state}` : "…"}{inFlight ? ` · ${inFlight} in flight` : ""}</span>
      </div>

      {error && <div className="empty" style={{ color: "var(--bad-fg)" }}>{error}</div>}

      <div className="card">
        <table className="tbl">
          <thead><tr><th>Issue</th><th>Labels</th><th>Workflow</th><th>Last run</th><th>Attention</th><th /></tr></thead>
          <tbody>
            {(rows ?? []).map((row) => {
              const a = attention(row);
              const busy = action?.ref === row.ref;
              return (
                <tr key={row.ref} className={a.tone === "ok" ? "eligible" : undefined}>
                  <td>
                    <div className="row-title">
                      {row.number != null ? <Link className="t-strong" to={issueSummaryPath(row.project, row.number)} style={{ color: "var(--fg)" }}>{row.title}</Link> : <b>{row.title}</b>}
                      <span className="mono">{row.project} #{row.number ?? "—"}</span>
                    </div>
                  </td>
                  <td>{row.labels.length === 0 ? <span className="dim">—</span> : row.labels.map((l) => <span className="label-chip" key={l}>{l}</span>)}</td>
                  <td><span className="mono dim">{row.workflow ?? "default"}</span></td>
                  <td className="mono dim">{row.last_run_ref ? (row.last_run_number != null ? `cycle ${row.last_run_number}` : row.last_run_state ?? "—") : "—"}</td>
                  <td><span title={a.detail ?? undefined}><Pill tone={a.tone}>{a.label}</Pill></span></td>
                  <td className="cell-actions">
                    {row.state === "open" ? (
                      <>
                        <button
                          className="btn btn-sm btn-primary"
                          onClick={() => void dispatch(row)}
                          disabled={row.issue_lock_held || !signedIn || (busy && action?.kind === "dispatching")}
                          title={!signedIn ? "sign in to dispatch" : undefined}
                        >
                          <Icon name="arrow" />
                          {row.issue_lock_held ? "in flight" : !signedIn ? "sign in" : busy && action?.kind === "dispatching" ? "dispatching…" : "Dispatch"}
                        </button>
                        <button className="btn btn-sm" onClick={() => void discard(row)} disabled={row.issue_lock_held || !signedIn || (busy && action?.kind === "discarding")}>
                          {busy && action?.kind === "discarding" ? "…" : "Discard"}
                        </button>
                        {busy && action?.kind === "error" && <span className="pill bad" title={action.message}>error</span>}
                        {busy && action?.kind === "done" && <span className="pill ok">dispatched</span>}
                      </>
                    ) : (
                      <span className="pill neutral">{row.state}</span>
                    )}
                  </td>
                </tr>
              );
            })}
            {rows && rows.length === 0 && (
              <tr><td colSpan={6}><div className="empty">No {state} issues across registered repos.</div></td></tr>
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}
