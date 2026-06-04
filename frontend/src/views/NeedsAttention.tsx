import { Link } from "react-router-dom";
import { Pill } from "../ui/bits";
import { attention, issueSummaryPath, useJson, type Attention, type IssueRow } from "./lib";

// Group the needs-attention feed by reason so the queue reads as "what kind of
// decision is waiting", matching the redesign's grouped layout.
const GROUPS: { key: string; title: string; match: (a: Attention) => boolean }[] = [
  { key: "review", title: "Review required", match: (a) => a.tone === "vio" },
  { key: "touchpoint", title: "Touchpoint ready", match: (a) => a.tone === "ok" },
  { key: "failed", title: "Failed runs", match: (a) => a.tone === "bad" },
  { key: "inflight", title: "In flight", match: (a) => a.tone === "warn" },
  { key: "other", title: "Other", match: () => true },
];

export function NeedsAttention() {
  const issues = useJson<IssueRow[]>("/v1/issues?needs_attention=true");
  const rows = issues.data ?? [];

  const assigned = new Set<string>();
  const grouped = GROUPS.map((g) => {
    const items = rows.filter((r) => !assigned.has(r.ref) && g.match(attention(r)));
    items.forEach((r) => assigned.add(r.ref));
    return { ...g, items };
  }).filter((g) => g.items.length > 0);

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Needs attention</h1>
          <div className="sub">Issues waiting on a decision — review, merge, or re-dispatch.</div>
        </div>
        <div className="page-actions"><Pill tone={rows.length ? "warn" : "ok"}>{rows.length} open</Pill></div>
      </div>

      {issues.error && <div className="empty" style={{ color: "var(--bad-fg)" }}>{issues.error}</div>}
      {!issues.loading && rows.length === 0 && <div className="empty">Nothing needs attention across registered repos.</div>}
      {issues.loading && rows.length === 0 && <div className="empty">Loading…</div>}

      <div className="stack">
        {grouped.map((g) => (
          <div className="card" key={g.key}>
            <div className="panel-head"><h3>{g.title}</h3><Pill tone={g.items[0] ? attention(g.items[0]).tone : "neutral"}>{g.items.length}</Pill></div>
            <table className="tbl">
              <tbody>
                {g.items.map((row) => {
                  const a = attention(row);
                  return (
                    <tr key={row.ref}>
                      <td style={{ width: "1%" }}><Pill tone={a.tone}>{a.label}</Pill></td>
                      <td>
                        <div className="row-title">
                          <b>{row.title}</b>
                          <span className="mono">{row.project} #{row.number} · {a.detail ?? row.state}</span>
                        </div>
                      </td>
                      <td>{row.labels.slice(0, 3).map((l) => <span className="label-chip" key={l}>{l}</span>)}</td>
                      <td style={{ width: "1%" }}>
                        {row.number != null && (
                          <Link className={`btn btn-sm${a.tone === "vio" || a.tone === "ok" ? " btn-primary" : ""}`} to={issueSummaryPath(row.project, row.number)}>
                            {a.tone === "vio" ? "Review" : a.tone === "ok" ? "Merge" : "Open"}
                          </Link>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ))}
      </div>
    </>
  );
}
