import { useNavigate } from "react-router-dom";
import { Icon } from "../ui/Icon";
import { Pill } from "../ui/bits";
import { issueReviewPath, runStateTone, useJson, usd, type ReviewRow, type Tone } from "./lib";

function prTone(row: ReviewRow): Tone {
  if (row.merged) return "neutral";
  if (row.state === "ready" || row.state === "needs_review") return "vio";
  if (row.state === "closed") return "neutral";
  return "warn";
}

export function Reviews() {
  const navigate = useNavigate();
  const { data, loading, error } = useJson<ReviewRow[]>("/v1/reviews");
  const rows = data ?? [];
  const ready = rows.filter((r) => !r.merged && (r.state === "ready" || r.state === "needs_review")).length;
  const inFlight = rows.filter((r) => r.pr_lock_held).length;

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Reviews</h1>
          <div className="sub">Pull requests opened by runs, waiting on your review or merge.</div>
        </div>
      </div>

      <div className="toolbar">
        <div className="spacer" />
        <span className="filter-note">{ready} ready · {inFlight} in flight</span>
      </div>

      {error && <div className="empty" style={{ color: "var(--bad-fg)" }}>{error}</div>}

      <div className="card">
        <table className="tbl">
          <thead><tr><th>Pull request</th><th>Issue</th><th>Run</th><th>Attempts</th><th>Cost</th><th>PR</th><th>Run state</th></tr></thead>
          <tbody>
            {rows.map((row) => (
              <tr
                key={row.ref}
                className={row.pr_lock_held ? "eligible" : undefined}
                style={{ cursor: row.issue_number != null ? "pointer" : "default" }}
                onClick={() => row.issue_number != null && navigate(issueReviewPath(row.project, row.issue_number))}
              >
                <td>
                  <div className="row-title">
                    <b>{row.title || `${row.repo}#${row.pr_number}`}</b>
                    <span className="mono">{row.repo} #{row.pr_number}{row.pr_branch ? ` · ${row.pr_branch}` : ""}</span>
                  </div>
                </td>
                <td className="mono dim">{row.issue_number != null ? `#${row.issue_number}` : "—"}</td>
                <td className="mono dim">{row.run_ref ? row.run_ref.slice(0, 8) : "—"}</td>
                <td className="mono dim">{row.run_attempts || "—"}</td>
                <td className="mono">{row.run_cumulative_cost_usd ? usd(row.run_cumulative_cost_usd) : "—"}</td>
                <td><Pill tone={prTone(row)}>{row.merged ? "merged" : row.state}</Pill></td>
                <td>{row.run_state ? <Pill tone={runStateTone(row.run_state)}>{row.run_state}</Pill> : <span className="dim fs-sm">manual</span>}</td>
              </tr>
            ))}
            {!loading && rows.length === 0 && <tr><td colSpan={7}><div className="empty">No reviews yet.</div></td></tr>}
            {loading && rows.length === 0 && <tr><td colSpan={7}><div className="empty">Loading…</div></td></tr>}
          </tbody>
        </table>
      </div>

      <div className="row gap-8 mt-12 fs-sm dim"><Icon name="pr" />Open a review to see evidence and reject-with-feedback.</div>
    </>
  );
}
