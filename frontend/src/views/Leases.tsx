import { useState } from "react";
import { Pill } from "../ui/bits";
import { authedFetch } from "../auth";
import { leaseKind, leaseStateTone, relTime, requesterLabel, ttlRemaining, useLayout } from "./lib";
import type { Lease, LeaseKind } from "../App";

export function Leases() {
  const { snap, isAdmin } = useLayout();
  const [tab, setTab] = useState<LeaseKind>("test");
  const [confirming, setConfirming] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const leases = (snap?.active_leases ?? []).filter((l) => leaseKind(l) === tab);
  const waiting = tab === "test" ? snap?.waiting_test_slot_requests ?? [] : [];

  const cancel = async (lease: Lease) => {
    setCancelling(lease.ref);
    setError(null);
    try {
      const r = await authedFetch("/v1/leases/cancel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ lease_ref: lease.ref }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setConfirming(null);
    } catch (e) {
      setError(String(e));
    } finally {
      setCancelling(null);
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Leases</h1>
          <div className="sub">The wedge primitive — a claim on a capability-matched resource, held for a TTL.</div>
        </div>
      </div>

      <div className="tabs">
        <div className={`tab${tab === "test" ? " on" : ""}`} onClick={() => setTab("test")}>test</div>
        <div className={`tab${tab === "agent" ? " on" : ""}`} onClick={() => setTab("agent")}>agent</div>
      </div>

      {error && <div className="empty mb-18" style={{ color: "var(--bad-fg)" }}>{error}</div>}

      <div className="card mb-18">
        <div className="panel-head"><h3>Active</h3><Pill tone={leases.length ? "warn" : "ok"}>{leases.length} held</Pill></div>
        {leases.length === 0 ? (
          <div className="card-pad"><div className="empty">No active {tab} leases.</div></div>
        ) : (
          <table className="tbl">
            <thead><tr><th>Lease</th><th>State</th><th>Resource</th><th>Requirements</th><th>TTL</th><th>Requester</th><th /></tr></thead>
            <tbody>
              {leases.map((l) => (
                <tr key={l.ref}>
                  <td className="mono t-strong">{l.ref.slice(0, 10)}…</td>
                  <td><Pill tone={leaseStateTone(l.state)}>{l.state}</Pill></td>
                  <td><div className="row-title"><b>{l.host ?? l.workflow ?? "—"}</b><span className="mono">{tab} lease{l.lease_number != null ? ` #${l.lease_number}` : ""}</span></div></td>
                  <td className="mono dim fs-sm" style={{ maxWidth: 220, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{JSON.stringify(l.requirements ?? {})}</td>
                  <td className="mono">{ttlRemaining(l)}</td>
                  <td className="mono dim">{requesterLabel(l.requester)}</td>
                  <td className="cell-actions">
                    {confirming === l.ref ? (
                      <div className="confirm">
                        <span className="q">cancel?</span>
                        <button className="btn btn-sm btn-danger" disabled={cancelling === l.ref} onClick={() => void cancel(l)}>{cancelling === l.ref ? "…" : "Yes"}</button>
                        <button className="btn btn-sm" onClick={() => setConfirming(null)}>Keep</button>
                      </div>
                    ) : (
                      <button className="btn btn-sm btn-danger" disabled={!isAdmin} title={!isAdmin ? "admin only" : undefined} onClick={() => setConfirming(l.ref)}>Cancel</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {tab === "test" && (
        <div className="card">
          <div className="panel-head"><h3>Waiting</h3><Pill tone={waiting.length ? "info" : "neutral"}>{waiting.length} queued</Pill></div>
          {waiting.length === 0 ? (
            <div className="card-pad"><div className="empty">No queued checkout requests.</div></div>
          ) : (
            <table className="tbl">
              <thead><tr><th>Request</th><th>State</th><th>Project</th><th>Workflow</th><th>Waiting</th><th>Requester</th></tr></thead>
              <tbody>
                {waiting.map((w) => (
                  <tr key={w.ref}>
                    <td className="mono t-strong">{w.ref.slice(0, 10)}…</td>
                    <td><Pill tone="info">{w.state}</Pill></td>
                    <td>{w.project}</td>
                    <td className="mono dim">{w.workflow}</td>
                    <td className="mono dim">{relTime(w.requested_at)}</td>
                    <td className="mono dim">{requesterLabel(w.requester)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </>
  );
}
