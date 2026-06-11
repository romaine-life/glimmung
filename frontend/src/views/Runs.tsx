import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Pill, Token } from "../ui/bits";
import { isMockMode, mockRuns } from "../mockApi";
import { durationLabel, relTime, runStateTone, useLayout, usd, type Tone } from "./lib";
import type { ProjectRun, RunReport } from "../App";

type RunRow = {
  key: string;
  project: string;
  issueNumber: number;
  title: string | null;
  workflow: string;
  runLabel: string;
  phase: string;
  cycle: string;
  attempts: string;
  cost: number;
  state: string;
  abortReason: string | null;
  started: string;
  completed: string | null;
};

function fromReport(r: RunReport): RunRow {
  return {
    key: r.ref,
    project: r.project,
    issueNumber: r.issue_number,
    title: null,
    workflow: r.workflow,
    runLabel: r.run_display_number ?? `${r.run_number ?? "?"}.${r.run_cycle_number ?? 1}`,
    phase: r.current_phase ?? "—",
    cycle: String(r.run_cycle_number ?? r.cycle_number ?? "—"),
    attempts: String(r.attempts_count ?? "—"),
    cost: r.cumulative_cost_usd ?? 0,
    state: r.state,
    abortReason: r.abort_reason ?? null,
    started: r.started_at,
    completed: r.completed_at,
  };
}

function fromProjectRun(r: ProjectRun): RunRow {
  return {
    key: r.id,
    project: r.project,
    issueNumber: r.issue_number,
    title: r.title,
    workflow: r.workflow,
    runLabel: r.run_display_number ?? `${r.run_number ?? "?"}.${r.run_cycle_number ?? 1}`,
    phase: r.current_phase,
    cycle: String(r.run_cycle_number ?? r.cycle_number ?? "—"),
    attempts: String(r.cycles ?? "—"),
    cost: r.cost_usd ?? 0,
    state: r.state,
    abortReason: null,
    started: r.started_at,
    completed: r.completed_at ?? null,
  };
}

const ACTIVE = new Set(["in_progress", "queued", "pending"]);

export function Runs() {
  const { snap } = useLayout();
  const projects = snap?.projects ?? [];
  const [rows, setRows] = useState<RunRow[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (isMockMode()) {
      setRows((mockRuns as ProjectRun[]).map(fromProjectRun));
      setLoading(false);
      return;
    }
    if (projects.length === 0) return;
    let cancelled = false;
    setLoading(true);
    Promise.all(
      projects.map((p) =>
        fetch(`/v1/projects/${encodeURIComponent(p.name)}/runs?limit=100`)
          .then((r) => (r.ok ? (r.json() as Promise<RunReport[]>) : []))
          .catch(() => [] as RunReport[]),
      ),
    ).then((all) => {
      if (cancelled) return;
      const merged = all.flat().map(fromReport).sort((a, b) => (a.started < b.started ? 1 : -1));
      setRows(merged);
      setLoading(false);
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projects.length]);

  const counts = {
    inProgress: rows.filter((r) => r.state === "in_progress").length,
    review: rows.filter((r) => r.state === "review_required" || r.state === "needs_review").length,
    passed: rows.filter((r) => r.state === "passed").length,
    aborted: rows.filter((r) => r.state === "aborted" || r.state === "failed").length,
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Runs</h1>
          <div className="sub">Every dispatched run and where it sits in the verify-loop.</div>
        </div>
      </div>

      <div className="kpis mb-20">
        <Kpi label="In progress" val={counts.inProgress} />
        <Kpi label="Review required" val={counts.review} />
        <Kpi label="Passed" val={counts.passed} />
        <Kpi label="Aborted" val={counts.aborted} />
      </div>

      <div className="card">
        <table className="tbl">
          <thead><tr><th>Run</th><th>Issue</th><th>Workflow</th><th>Phase</th><th>Cycle</th><th>Attempts</th><th>Cost</th><th>State</th><th>Started</th><th>Duration</th></tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key}>
                <td><Link className="link mono" to={`/projects/${encodeURIComponent(r.project)}/issues/${r.issueNumber}/runs`}>{r.runLabel}</Link></td>
                <td>
                  <div className="row-title">
                    <b>{r.title ?? `#${r.issueNumber}`}</b>
                    <span className="mono">{r.project} #{r.issueNumber}</span>
                  </div>
                </td>
                <td className="mono dim">{r.workflow}</td>
                <td><span className={`mono ${ACTIVE.has(r.state) ? "accent" : "dim"}`}>{r.phase}</span></td>
                <td className="mono dim">{r.cycle}</td>
                <td className="mono dim">{r.attempts}</td>
                <td className="mono t-strong">{usd(r.cost)}</td>
                <td>
                  <span className="row gap-6">
                    <Pill tone={runStateTone(r.state)} live={r.state === "in_progress"}>{r.state}</Pill>
                    {r.abortReason && <Token kind="abort">{r.abortReason.toUpperCase()}</Token>}
                  </span>
                </td>
                <td className="dim">{relTime(r.started)}</td>
                <td className="mono dim">{durationLabel(r.started, r.completed, r.state)}</td>
              </tr>
            ))}
            {!loading && rows.length === 0 && <tr><td colSpan={10}><div className="empty">No runs yet.</div></td></tr>}
            {loading && rows.length === 0 && <tr><td colSpan={10}><div className="empty">Loading…</div></td></tr>}
          </tbody>
        </table>
      </div>
    </>
  );
}

function Kpi({ label, val }: { label: string; val: number }) {
  const tone: Tone = label === "Aborted" && val > 0 ? "bad" : label === "Review required" && val > 0 ? "vio" : "neutral";
  return (
    <div className="card kpi">
      <div className="kpi-label">{label}</div>
      <div className="kpi-val" style={tone === "bad" ? { color: "var(--bad-fg)" } : tone === "vio" ? { color: "var(--vio-fg)" } : undefined}>{val}</div>
    </div>
  );
}
