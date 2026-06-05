import { useLayoutEffect, useRef } from "react";
import type { ReactNode } from "react";
import { Icon } from "./Icon";

export type GraphJob = { title: ReactNode; sub?: ReactNode; state?: "done" | "active" | "pending" };

// Mirrors the real PhaseSpec.recycle_policy / PR-primitive recycle_policy.
// The graph shows ONE traversal; a recycle is a POLICY annotation meaning
// "on these signals a NEW run lands at <landsAt>" — never an in-run loop.
export type GraphRecycle = { on: string[]; landsAt: string; maxAttempts: number };

export type GraphPhase = {
  name: string;
  kind: string;
  state?: "done" | "active" | "pending";
  status?: ReactNode;
  // A phase holds one or more JOBS that run in PARALLEL — rendered stacked
  // vertically within the phase's column (issue → run → phase → job → step).
  jobs?: GraphJob[];
  recycle?: GraphRecycle;
};

// [sourcePhase, targetPhase, label] — low-level edge form. Prefer per-phase
// `recycle` policy (above); this stays as a fallback/override.
export type Recycle = [string, string, string];

const SVGNS = "http://www.w3.org/2000/svg";

function displayIdentifier(value: string): string {
  const normalized = value.trim().replace(/[_-]+/g, " ").replace(/\s+/g, " ");
  if (!normalized) return value;
  return normalized.charAt(0).toUpperCase() + normalized.slice(1);
}

export function PhaseGraph({
  phases,
  recycles,
}: {
  phases: GraphPhase[];
  recycles?: Recycle[];
}) {
  // Data-driven: derive recycle arrows from each phase's policy. Fall back to an
  // explicit `recycles` prop, then to the default verify→implement.
  const derived: Recycle[] = phases.flatMap((p) =>
    p.recycle
      ? [[p.name, p.recycle.landsAt, `${p.recycle.on.join("/")} ×${p.recycle.maxAttempts}`] as Recycle]
      : [],
  );
  const arrows: Recycle[] = derived.length ? derived : recycles ?? [["verify", "implement", "reject ×2"]];
  const innerRef = useRef<HTMLDivElement>(null);
  const trackRef = useRef<HTMLDivElement>(null);
  const svgRef = useRef<SVGSVGElement>(null);

  useLayoutEffect(() => {
    const inner = innerRef.current;
    const track = trackRef.current;
    const svg = svgRef.current;
    if (!inner || !track || !svg) return;

    const idxOf = (name: string) => {
      const els = track.children;
      for (let i = 0; i < els.length; i++) {
        if ((els[i] as HTMLElement).dataset.phase === name) return i;
      }
      return -1;
    };

    const draw = () => {
      const nodes = Array.from(track.children) as HTMLElement[];
      if (!nodes.length || !nodes[0].offsetWidth) return;

      const H = nodes[0].offsetHeight;
      const top = nodes[0].offsetTop;
      const cy = top + H / 2;
      const laneGap = 30;
      inner.style.paddingBottom = `${28 + arrows.length * laneGap}px`;
      const W = inner.offsetWidth;
      const Ht = inner.offsetHeight;

      svg.querySelectorAll("path.edge").forEach((p) => p.remove());
      inner.querySelectorAll(".recycle-label").forEach((l) => l.remove());
      svg.setAttribute("width", String(W));
      svg.setAttribute("height", String(Ht));
      svg.setAttribute("viewBox", `0 0 ${W} ${Ht}`);

      const add = (d: string, recycle: boolean, marker: string) => {
        const p = document.createElementNS(SVGNS, "path");
        p.setAttribute("d", d);
        p.setAttribute("class", `edge ${recycle ? "edge-rec" : "edge-adv"}`);
        p.setAttribute("marker-end", `url(#${marker})`);
        svg.appendChild(p);
      };

      const L = nodes.map((n) => n.offsetLeft);
      const R = nodes.map((n) => n.offsetLeft + n.offsetWidth);
      const C = nodes.map((n) => n.offsetLeft + n.offsetWidth / 2);

      for (let j = 0; j < nodes.length - 1; j++) {
        const x1 = R[j], x2 = L[j + 1], mid = (x1 + x2) / 2;
        add(`M ${x1},${cy} C ${mid},${cy} ${mid},${cy} ${x2 - 3},${cy}`, false, "arrow");
      }

      const pb = top + H, r = 9;
      arrows.forEach((rc, li) => {
        const si = idxOf(rc[0]), ti = idxOf(rc[1]);
        if (si < 0 || ti < 0) return;
        const sx = C[si], tx = C[ti];
        const laneY = pb + 24 + li * laneGap;
        add(
          `M ${sx},${pb} L ${sx},${laneY - r} Q ${sx},${laneY} ${sx - r},${laneY} ` +
            `L ${tx + r},${laneY} Q ${tx},${laneY} ${tx},${laneY - r} L ${tx},${pb + 3}`,
          true,
          "arrow-r",
        );
        const lab = document.createElement("div");
        lab.className = "recycle-label";
        lab.innerHTML = `<svg class="ic" style="width:11px;height:11px;vertical-align:-1px"><use href="#ic-repeat"></use></svg> ${rc[2]}`;
        lab.style.left = `${(sx + tx) / 2 - 34}px`;
        lab.style.top = `${laneY - 9}px`;
        inner.appendChild(lab);
      });
    };

    draw();
    const ro = new ResizeObserver(draw);
    ro.observe(inner);
    window.addEventListener("resize", draw);
    if (document.fonts?.ready) document.fonts.ready.then(draw);
    const timers = [60, 200, 600].map((t) => window.setTimeout(draw, t));
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", draw);
      timers.forEach(clearTimeout);
    };
  }, [phases]);

  return (
    <div className="dag">
      <div className="dag-inner" ref={innerRef}>
        <svg className="dag-edges" ref={svgRef}>
          <defs>
            <marker id="arrow" markerWidth="9" markerHeight="9" refX="7" refY="4.5" orient="auto">
              <path d="M1,1 L8,4.5 L1,8" fill="none" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
            </marker>
            <marker id="arrow-r" markerWidth="9" markerHeight="9" refX="7" refY="4.5" orient="auto">
              <path d="M1,1 L8,4.5 L1,8" fill="none" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" />
            </marker>
          </defs>
        </svg>
        <div className="dag-track" ref={trackRef}>
          {phases.map((p) => (
            <div
              key={p.name}
              className={`phase${p.state ? ` is-${p.state}` : ""}`}
              data-phase={p.name}
            >
              {p.status && <div className="phase-status">{p.status}</div>}
              <div className="phase-head">
                <span className="pn" title={p.name}>{displayIdentifier(p.name)}</span>
                <span className="pk">{p.kind}</span>
              </div>
              <div className="phase-body">
                {(p.jobs ?? [{ title: p.name }]).map((job, ji) => (
                  <div className="job-box" key={ji}>
                    <div className="jt">
                      {job.state === "done" && <Icon name="check" style={{ width: 13, height: 13, color: "var(--ok-fg)" }} />}
                      {job.state === "active" && <Icon name="loader" style={{ width: 13, height: 13, color: "var(--accent)" }} />}
                      {job.title}
                    </div>
                    {job.sub && <div className="js">{job.sub}</div>}
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
