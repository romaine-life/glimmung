import type { ReactNode } from "react";

type Tone = "ok" | "warn" | "bad" | "info" | "vio" | "neutral";

export function Pill({ tone, live, children }: { tone: Tone; live?: boolean; children: ReactNode }) {
  return (
    <span className={`pill ${tone}${live ? " live" : ""}`}>
      <span className="pdot" />
      {children}
    </span>
  );
}

export function Token({ kind, children }: { kind: "advance" | "retry" | "abort"; children: ReactNode }) {
  return <span className={`token ${kind}`}>{children}</span>;
}

export function Meter({ pct, tone }: { pct: number; tone?: "warn" | "bad" }) {
  return (
    <div className="meter">
      <i className={tone ?? ""} style={{ width: `${pct}%` }} />
    </div>
  );
}
