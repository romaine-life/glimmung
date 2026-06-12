import { Link } from "react-router-dom";
import { Icon } from "../ui/Icon";
import { Pill } from "../ui/bits";
import {
  attention,
  issueSummaryPath,
  issueTouchpointPath,
  leaseKind,
  leaseStateTone,
  slotStateTone,
  ttlRemaining,
  useJson,
  useLayout,
  usd,
  type IssueRow,
  type TouchpointRow,
} from "./lib";

export function Overview() {
  const { snap } = useLayout();
  const issues = useJson<IssueRow[]>("/v1/issues?needs_attention=true");
  const touchpoints = useJson<TouchpointRow[]>("/v1/touchpoints");

  const leases = snap?.active_leases ?? [];
  const slots = snap?.test_environments ?? [];
  const projects = snap?.projects ?? [];
  const workflows = snap?.workflows ?? [];
  const testLeases = leases.filter((l) => leaseKind(l) === "test").length;
  const agentLeases = leases.length - testLeases;
  const busySlots = slots.filter((s) => s.lease != null || ["claimed", "active", "running"].includes(s.state)).length;
  const waiting = (snap?.waiting_test_slot_requests ?? []).length + slots.reduce((n, s) => n + (s.waiting_requests?.length ?? 0), 0);
  const attentionRows = issues.data ?? [];
  const openTouchpoints = (touchpoints.data ?? []).filter((t) => !t.merged && t.state !== "closed");

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Overview</h1>
          <div className="sub">From a thousand worlds they came, each with a craft to contribute.</div>
        </div>
      </div>

      <div className="kpis">
        <div className="card kpi clickable">
          <Link to="/leases" className="card-link-overlay" aria-label="Active leases" />
          <div className="kpi-label">Active leases</div>
          <div className="kpi-val">{leases.length}</div>
          <div className="kpi-foot">
            <Icon name="lease" />
            <Link to="/leases" state={{ tab: "test" }} className="kpi-link">
              {testLeases} test
            </Link>
            {" · "}
            <Link to="/leases" state={{ tab: "agent" }} className="kpi-link">
              {agentLeases} agent
            </Link>
          </div>
        </div>
        <div className="card kpi">
          <div className="kpi-label">Needs review</div>
          <div className="kpi-val">{issues.loading ? "—" : attentionRows.length}</div>
          <div className="kpi-foot">
            {openTouchpoints.length > 0 ? <Pill tone="vio">{openTouchpoints.length} touchpoints</Pill> : <span className="dim">no open touchpoints</span>}
          </div>
        </div>
        <div className="card kpi">
          <div className="kpi-label">Test slots</div>
          <div className="kpi-val">{busySlots}<small> / {slots.length} busy</small></div>
          <div className="kpi-foot"><Icon name="flask" />{waiting} waiting</div>
        </div>
        <div className="card kpi">
          <div className="kpi-label">Projects</div>
          <div className="kpi-val">{projects.length}</div>
          <div className="kpi-foot"><Icon name="workflow" />{workflows.length} workflows</div>
        </div>
      </div>

      <div className="grid-2">
        <div className="stack">
          <div className="card">
            <div className="panel-head">
              <h3>Needs attention</h3>
              {attentionRows.length > 0 && <Pill tone="warn">{attentionRows.length}</Pill>}
              <div className="panel-actions"><Link className="link fs-sm" to="/needs-attention">View all</Link></div>
            </div>
            {attentionRows.length === 0 ? (
              <div className="card-pad"><div className="empty">{issues.loading ? "Loading…" : "Nothing needs attention."}</div></div>
            ) : (
              <table className="tbl">
                <tbody>
                  {attentionRows.slice(0, 6).map((row) => {
                    const a = attention(row);
                    return (
                      <tr key={row.ref}>
                        <td style={{ width: "1%" }}><Pill tone={a.tone}>{a.label}</Pill></td>
                        <td>
                          <div className="row-title">
                            <b>{row.title}</b>
                            <span>{row.project} #{row.number} · {a.detail ?? row.state}</span>
                          </div>
                        </td>
                        <td style={{ width: "1%" }}>
                          {row.number != null && (
                            <Link className={`btn btn-sm${a.tone === "vio" || a.tone === "ok" ? " btn-primary" : ""}`} to={issueSummaryPath(row.project, row.number)}>
                              Open
                            </Link>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>

          <div className="card">
            <div className="panel-head">
              <h3>Active leases</h3>
              <div className="panel-actions"><Link className="link fs-sm" to="/leases">All leases</Link></div>
            </div>
            {leases.length === 0 ? (
              <div className="card-pad"><div className="empty">No active leases.</div></div>
            ) : (
              <table className="tbl">
                <thead><tr><th>Lease</th><th>Project</th><th>Resource</th><th>State</th><th>TTL</th></tr></thead>
                <tbody>
                  {leases.slice(0, 6).map((l) => (
                    <tr key={l.ref}>
                      <td className="mono t-strong">{l.ref.slice(0, 8)}…</td>
                      <td>{l.project}</td>
                      <td className="mono dim">{l.host ?? l.workflow ?? leaseKind(l)}</td>
                      <td><Pill tone={leaseStateTone(l.state)}>{l.state}</Pill></td>
                      <td className="mono dim">{ttlRemaining(l)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>

        <div className="stack">
          <div className="card">
            <div className="panel-head">
              <h3>Test slots</h3>
              <Pill tone={busySlots ? "warn" : "ok"}>{busySlots} / {slots.length} busy</Pill>
              <div className="panel-actions"><Link className="link fs-sm" to="/test-slots">Manage</Link></div>
            </div>
            <div className="card-pad col gap-10">
              {slots.length === 0 ? <div className="empty">No test slots configured.</div> : slots.map((s) => (
                <div className="row gap-10" key={s.slot_name}>
                  <Icon name="flask" style={{ color: "var(--fg-dim)" }} />
                  <div className="row-title grow">
                    <b>{s.slot_name}</b>
                    <span className="mono">{s.lease ? `claimed · ${ttlRemaining(s.lease)} ttl` : s.detail ?? s.state}</span>
                  </div>
                  <Pill tone={slotStateTone(s.state)}>{s.state || "—"}</Pill>
                </div>
              ))}
            </div>
          </div>

          <div className="card">
            <div className="panel-head"><h3>Touchpoints</h3><div className="panel-actions"><Link className="link fs-sm" to="/touchpoints">All</Link></div></div>
            <div style={{ padding: 6 }}>
              {openTouchpoints.length === 0 ? (
                <div className="card-pad"><div className="empty">No open touchpoints.</div></div>
              ) : openTouchpoints.slice(0, 4).map((t) => (
                <Link
                  key={t.ref}
                  className="ev"
                  to={t.issue_number != null ? issueTouchpointPath(t.project, t.issue_number) : "/touchpoints"}
                  style={{ margin: 6, border: "none", background: "transparent" }}
                >
                  <Icon name="pr" />
                  <div className="ev-main"><b>{t.title || `${t.repo}#${t.pr_number}`}</b><span>{t.repo} #{t.pr_number} · {t.state}</span></div>
                  <span className="mla">
                    {t.run_state ? <Pill tone={t.run_state === "review_required" ? "vio" : t.run_state === "passed" ? "ok" : "warn"}>{t.run_state}</Pill> : <span className="dim fs-xs">manual</span>}
                  </span>
                </Link>
              ))}
            </div>
          </div>

          <div className="card card-pad">
            <div className="eyebrow mb-10">Spend</div>
            <div className="grid-kv">
              <div className="k">open touchpoints</div><div className="v mono fs-sm">{usd(openTouchpoints.reduce((n, t) => n + (t.run_cumulative_cost_usd || 0), 0))}</div>
              <div className="k">projects</div><div className="v mono fs-sm">{projects.map((p) => p.name).join(", ") || "—"}</div>
            </div>
          </div>
        </div>
      </div>
    </>
  );
}
