import { Link } from "react-router-dom";
import { Icon } from "../ui/Icon";
import { Pill } from "../ui/bits";
import { PhaseGraph, type GraphPhase } from "../ui/PhaseGraph";
import { useLayout } from "./lib";
import type { PhaseSpec, RecyclePolicy, Workflow } from "../App";

function recycleOf(policy: RecyclePolicy | null | undefined): GraphPhase["recycle"] {
  if (!policy) return undefined;
  return { on: policy.on, landsAt: policy.lands_at, maxAttempts: policy.max_attempts };
}

function phaseModel(wf: Workflow): GraphPhase[] {
  const phases: GraphPhase[] = wf.phases.map((p: PhaseSpec) => ({
    name: p.name,
    kind: p.kind,
    jobs: (p.jobs ?? []).map((j) => ({
      title: j.name ?? j.id,
      sub: j.primitive ?? (j.steps?.length ? `${j.steps.length} steps` : undefined),
    })),
    recycle: recycleOf(p.recycle_policy),
  }));
  // The PR primitive's recycle lands a fresh run at its target on changes_requested.
  // Attach it to the touchpoint phase (its source) so the graph draws that arc.
  const pr = recycleOf(wf.pr?.recycle_policy);
  if (pr) {
    const tp = phases.find((p) => /touchpoint|pr/i.test(p.name) || /touchpoint|pr/i.test(p.kind));
    if (tp && !tp.recycle) tp.recycle = pr;
  }
  return phases;
}

export function Workflows() {
  const { snap } = useLayout();
  const workflows = snap?.workflows ?? [];

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Workflows</h1>
          <div className="sub">Phase definitions, requirements, and the recycle policy that re-lands runs on rejection.</div>
        </div>
      </div>

      {workflows.length === 0 && <div className="empty">No workflows registered.</div>}

      <div className="stack">
        {workflows.map((wf) => (
          <div className="card" key={`${wf.project}/${wf.name}`}>
            <div className="panel-head">
              <h3>{wf.name}</h3>
              <Pill tone="info">{wf.phases.length} phases</Pill>
              <span className="id-chip">{wf.project}</span>
              <div className="panel-actions">
                <Link className="btn btn-sm" to={`/projects/${encodeURIComponent(wf.project)}/workflows/${encodeURIComponent(wf.name)}`}>
                  <Icon name="ext" />Open
                </Link>
              </div>
            </div>
            <PhaseGraph phases={phaseModel(wf)} />
          </div>
        ))}
      </div>
    </>
  );
}
