import { useState } from "react";
import { Link } from "react-router-dom";
import { Icon } from "../ui/Icon";
import { Pill } from "../ui/bits";
import { relTime, slotStateTone, ttlRemaining, useLayout } from "./lib";

function dotTone(event: string): string {
  if (/clean/i.test(event)) return "warn";
  if (/return|release/i.test(event)) return "ok";
  if (/claim|checkout/i.test(event)) return "accent";
  if (/error|expire/i.test(event)) return "warn";
  return "";
}

export function TestSlots() {
  const { snap } = useLayout();
  const slots = snap?.test_environments ?? [];
  const [selName, setSelName] = useState<string | null>(null);
  const sel = slots.find((s) => s.slot_name === selName) ?? slots[0] ?? null;

  const claimed = slots.filter((s) => s.lease != null || ["claimed", "active", "running"].includes(s.state)).length;
  const available = slots.filter((s) => s.state === "available" || s.state === "provisioned").length;
  const waiting = (snap?.waiting_test_slot_requests ?? []).length + slots.reduce((n, s) => n + (s.waiting_requests?.length ?? 0), 0);
  const history = sel?.test_slot_return_history ?? [];

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Test slots</h1>
          <div className="sub">Self-hosted environments leased by verify phases. Each holds a live Playwright endpoint.</div>
        </div>
      </div>

      <div className="kpis mb-20">
        <div className="card kpi"><div className="kpi-label">Total slots</div><div className="kpi-val">{slots.length}</div></div>
        <div className="card kpi"><div className="kpi-label">Claimed</div><div className="kpi-val">{claimed}</div></div>
        <div className="card kpi"><div className="kpi-label">Available</div><div className="kpi-val">{available}</div></div>
        <div className="card kpi"><div className="kpi-label">Waiting requests</div><div className="kpi-val">{waiting}</div></div>
      </div>

      {slots.length === 0 ? (
        <div className="empty">No test slots configured.</div>
      ) : (
        <div className="grid-2">
          <div className="card">
            <div className="panel-head"><h3>Slots</h3></div>
            <table className="tbl">
              <thead><tr><th>Slot</th><th>State</th><th>Activation</th><th>Lease</th><th>Waiting</th></tr></thead>
              <tbody>
                {slots.map((s) => (
                  <tr key={s.slot_name} className={`slot-row${sel?.slot_name === s.slot_name ? " sel" : ""}`} onClick={() => setSelName(s.slot_name)}>
                    <td className="t-strong mono">{s.slot_name}</td>
                    <td><Pill tone={slotStateTone(s.state)}>{s.state || "—"}</Pill></td>
                    <td className="mono dim">{s.activation_state ?? (s.usable ? "ready" : "—")}</td>
                    <td>{s.lease ? <span className="mono accent">{s.lease.ref.slice(0, 8)}…</span> : <span className="dim">—</span>}</td>
                    <td>{s.waiting_requests?.length ? <span className="mono" style={{ color: "var(--info-fg)" }}>{s.waiting_requests.length}</span> : <span className="dim">—</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <div className="stack">
            {sel && (
              <div className="card">
                <div className="panel-head"><h3>{sel.slot_name}</h3><Pill tone={slotStateTone(sel.state)}>{sel.state || "—"}</Pill></div>
                <div className="card-pad">
                  <div className="grid-kv">
                    <div className="k">activation</div><div className="v"><span className={`pill ${sel.usable ? "ok" : "info"}`} style={{ fontSize: 10 }}>{sel.activation_state ?? (sel.usable ? "ready" : "pending")}</span></div>
                    <div className="k">cleanup</div><div className="v"><span className="pill neutral" style={{ fontSize: 10 }}>{sel.cleanup_state ?? "idle"}</span></div>
                    <div className="k">lease</div><div className="v mono fs-sm">{sel.lease ? `${sel.lease.ref.slice(0, 12)}… · ${ttlRemaining(sel.lease)}` : "—"}</div>
                    <div className="k">updated</div><div className="v mono fs-sm">{relTime(sel.updated_at)}</div>
                    <div className="k">endpoint</div><div className="v mono fs-sm" style={{ wordBreak: "break-all" }}>{sel.playwright_ws_endpoint ?? "—"}</div>
                  </div>
                  <div className="row gap-10 mt-16">
                    <Link className="btn btn-sm" to={`/projects/${encodeURIComponent(sel.project)}/leases/test/slots/${sel.slot_index}`}>
                      <Icon name="ext" />Slot detail
                    </Link>
                  </div>
                </div>
              </div>
            )}

            <div className="card">
              <div className="panel-head"><h3>Return history</h3></div>
              <div className="card-pad">
                {history.length === 0 ? (
                  <div className="empty">No return history for this slot.</div>
                ) : (
                  <div className="timeline">
                    {history.map((h, i) => (
                      <div className="tl-item" key={`${h.created_at}:${i}`}>
                        <div className={`tl-dot ${dotTone(h.event)}`} />
                        <div className="tl-body">
                          <b>{h.event}{h.lease_number != null ? ` · lease #${h.lease_number}` : ""}</b>
                          <span>{relTime(h.created_at)} · {h.source}{h.reason ? ` · ${h.reason}` : ""}</span>
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>
          </div>
        </div>
      )}
    </>
  );
}
