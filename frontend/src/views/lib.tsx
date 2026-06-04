// Shared helpers for the redesigned ("Heldscalla") views. Each view reads the
// live SSE snapshot through `useLayout()` (the Layout Outlet context) and/or
// fetches its own detail data with `useJson`. No Shell here — Layout owns the
// shell chrome; views render page content only.
import { useCallback, useEffect, useState } from "react";
import { useOutletContext } from "react-router-dom";
import type { LayoutContext, Lease, LeaseKind } from "../App";

export type Tone = "ok" | "warn" | "bad" | "info" | "vio" | "neutral";

export function useLayout(): LayoutContext {
  return useOutletContext<LayoutContext>();
}

// ---- live API row shapes (mirror the server /v1 contracts) ----
export type IssueRow = {
  ref: string;
  project: string;
  workflow: string | null;
  repo: string | null;
  number: number | null;
  title: string;
  state: string;
  labels: string[];
  html_url: string | null;
  last_run_ref: string | null;
  last_run_number: number | null;
  last_run_state: string | null;
  last_run_abort_reason: string | null;
  issue_lock_held: boolean;
};

export type TouchpointRow = {
  ref: string;
  project: string;
  repo: string;
  pr_number: number;
  pr_branch: string | null;
  title: string;
  state: string;
  merged: boolean;
  html_url: string | null;
  linked_issue_ref: string | null;
  linked_run_ref: string | null;
  issue_number: number | null;
  run_ref: string | null;
  run_state: string | null;
  run_attempts: number;
  run_cumulative_cost_usd: number;
  pr_lock_held: boolean;
};

// ---- formatting ----
export function relTime(value?: string | null): string {
  if (!value) return "—";
  const t = new Date(value).getTime();
  if (Number.isNaN(t)) return "—";
  const s = Math.round((Date.now() - t) / 1000);
  if (s < 0) return "just now";
  if (s < 60) return s <= 1 ? "just now" : `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

export function usd(n?: number | null): string {
  if (n == null || Number.isNaN(n)) return "—";
  return `$${n.toFixed(2)}`;
}

export function issueSummaryPath(project: string, number: number | string): string {
  return `/projects/${encodeURIComponent(project)}/issues/${number}/summary`;
}

export function issueTouchpointPath(project: string, number: number | string): string {
  return `/projects/${encodeURIComponent(project)}/issues/${number}/touchpoint`;
}

export function ttlRemaining(lease: Lease): string {
  if (!lease.assigned_at || !lease.ttl_seconds) return "—";
  const end = new Date(lease.assigned_at).getTime() + lease.ttl_seconds * 1000;
  const ms = end - Date.now();
  if (Number.isNaN(ms)) return "—";
  if (ms <= 0) return "expired";
  const m = Math.round(ms / 60000);
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

export function leaseKind(lease: Lease): LeaseKind {
  if (lease.workflow === "test-slot-checkout") return "test";
  if ((lease.metadata as Record<string, unknown> | null)?.test_slot_checkout === true) return "test";
  return "agent";
}

export function requesterLabel(value: Record<string, unknown> | null): string {
  if (!value) return "—";
  const v = value as Record<string, unknown>;
  const candidate = v.session_id ?? v.label ?? v.name ?? v.email ?? v.pod ?? v.requester;
  return candidate != null ? String(candidate) : "—";
}

// ---- state -> tone (the redesign's pill language) ----
export function runStateTone(state?: string | null): Tone {
  switch (state) {
    case "passed": return "ok";
    case "in_progress": return "warn";
    case "queued":
    case "pending": return "neutral";
    case "review_required":
    case "needs_review": return "vio";
    case "aborted":
    case "failed": return "bad";
    case "manual": return "info";
    default: return "neutral";
  }
}

export function leaseStateTone(state: string): Tone {
  switch (state) {
    case "active":
    case "claimed": return "warn";
    case "released": return "neutral";
    case "expired": return "bad";
    default: return "info";
  }
}

export function slotStateTone(state: string): Tone {
  switch (state) {
    case "available":
    case "provisioned": return "ok";
    case "claimed":
    case "active":
    case "running": return "warn";
    case "warming":
    case "provisioning":
    case "activating":
    case "cleaning": return "info";
    case "error": return "bad";
    default: return "neutral";
  }
}

export type Attention = { label: string; detail: string | null; tone: Tone };

// Faithful port of IssuesView.attentionReason, mapped to the new pill tones.
export function attention(row: IssueRow): Attention {
  if (row.state !== "open") {
    return { label: row.state, detail: row.last_run_state ? `last run ${row.last_run_state}` : null, tone: "info" };
  }
  if (row.issue_lock_held) {
    return { label: "run in flight", detail: row.last_run_number !== null ? `cycle ${row.last_run_number} holds the issue lock` : null, tone: "warn" };
  }
  if (!row.last_run_ref) {
    return { label: "not dispatched", detail: "open issue has not had an agent run yet", tone: "info" };
  }
  if (row.last_run_state === "aborted" || row.last_run_state === "failed") {
    return { label: "last run failed", detail: row.last_run_abort_reason, tone: "bad" };
  }
  if (row.last_run_state === "in_progress" || row.last_run_state === "queued" || row.last_run_state === "pending") {
    const queued = row.last_run_state === "queued" || row.last_run_state === "pending";
    return { label: queued ? "run queued" : "run still active", detail: row.last_run_number !== null ? `cycle ${row.last_run_number} is ${row.last_run_state}` : null, tone: queued ? "neutral" : "warn" };
  }
  if (row.last_run_state === "passed") {
    return { label: "touchpoint ready", detail: "agent run passed and is ready for review", tone: "ok" };
  }
  if (row.last_run_state === "needs_review" || row.last_run_state === "review_required") {
    return { label: "review needed", detail: null, tone: "vio" };
  }
  return { label: row.last_run_state ? `last run ${row.last_run_state}` : "open issue", detail: null, tone: "info" };
}

// ---- a tiny fetch hook (mock-mode safe: installMockFetch intercepts) ----
export function useJson<T>(url: string | null): {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
} {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(Boolean(url));
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (!url) {
      setData(null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetch(url, { credentials: "include" })
      .then(async (r) => {
        if (!r.ok) throw new Error(`${url} -> ${r.status}`);
        return (await r.json()) as T;
      })
      .then((d) => {
        if (!cancelled) {
          setData(d);
          setLoading(false);
        }
      })
      .catch((e) => {
        if (!cancelled) {
          setError(String(e));
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [url, nonce]);

  return { data, loading, error, reload };
}
