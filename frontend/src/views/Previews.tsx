import { useMemo, useState, type FormEvent } from "react";
import { Pill } from "../ui/bits";
import { Icon } from "../ui/Icon";
import { authedFetch } from "../auth";
import { previewStateTone, relTime, useLayout } from "./lib";
import type { PreviewEnvironment, Project } from "../App";

// The live-preview lane dashboard. It renders the durable, OBSERVED state of
// each preview_environment from the SSE snapshot (`snap.preview_environments`,
// resynced from the durable cursor on reconnect — no polling). Controls
// (provision / enable / disable / delete) fire the durable mutation and then
// wait for the snapshot to converge: a control is "complete" only when the
// durable row reflects it, never on optimistic click success.

function shortBuild(id: string): string {
  const v = (id ?? "").trim();
  if (!v) return "—";
  return v.length > 14 ? `${v.slice(0, 14)}…` : v;
}

// A preview is in the durable trust gap when a build was claimed (pushed) but
// the edge is NOT observed serving it. The durable state owns this; we only
// mirror it.
function isTrustGap(env: PreviewEnvironment): boolean {
  return env.state === "stale";
}

function livePreviewEnabled(project: Project): boolean {
  const lp = (project.metadata as Record<string, unknown> | null)?.live_preview;
  return !!lp && typeof lp === "object" && (lp as { enabled?: unknown }).enabled === true;
}

type Confirm = { kind: "disable" | "delete"; key: string } | null;

export function Previews() {
  const { snap, isAdmin } = useLayout();
  const envs = useMemo(
    () =>
      (snap?.preview_environments ?? [])
        .slice()
        .sort((a, b) => `${a.project}/${a.name}`.localeCompare(`${b.project}/${b.name}`)),
    [snap?.preview_environments],
  );
  const previewProjects = useMemo(
    () => (snap?.projects ?? []).filter(livePreviewEnabled).slice().sort((a, b) => a.name.localeCompare(b.name)),
    [snap?.projects],
  );

  const [selKey, setSelKey] = useState<string | null>(null);
  const key = (e: PreviewEnvironment) => `${e.project}/${e.name}`;
  const sel = envs.find((e) => key(e) === selKey) ?? null;

  const [busy, setBusy] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [error, setError] = useState<string | null>(null);

  // Provision form state.
  const [pProject, setPProject] = useState("");
  const [pName, setPName] = useState("");
  const [provisioning, setProvisioning] = useState(false);

  const counts = {
    total: envs.length,
    live: envs.filter((e) => e.state === "live").length,
    pushed: envs.filter((e) => e.state === "pushed").length,
    stale: envs.filter((e) => e.state === "stale").length,
    provisioning: envs.filter((e) => e.state === "provisioning" || e.state === "ready").length,
    disabled: envs.filter((e) => e.state === "disabled").length,
    error: envs.filter((e) => e.state === "error").length,
  };

  // A control is complete only when the durable row confirms it. We fire the
  // request, clear the local arming/busy UI, and let the SSE snapshot drive the
  // visible state — we never optimistically rewrite the row.
  const control = async (env: PreviewEnvironment, action: "enable" | "disable" | "delete") => {
    setBusy(`${key(env)}:${action}`);
    setError(null);
    try {
      const path = `/v1/previews/${encodeURIComponent(env.project)}/${encodeURIComponent(env.name)}`;
      const r =
        action === "delete"
          ? await authedFetch(path, { method: "DELETE" })
          : await authedFetch(`${path}/${action}`, { method: "POST" });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setConfirm(null);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(null);
    }
  };

  const provision = async (e: FormEvent) => {
    e.preventDefault();
    if (!pProject) return;
    setProvisioning(true);
    setError(null);
    try {
      const r = await authedFetch("/v1/previews", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ project: pProject, name: pName.trim() }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      // Durable row lands as `provisioning` and arrives via SSE — clear the
      // form, do not synthesize a row locally.
      setPName("");
    } catch (e) {
      setError(String(e));
    } finally {
      setProvisioning(false);
    }
  };

  if (snap === null) return <div className="empty">Connecting…</div>;

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Previews</h1>
          <div className="sub">
            the live-preview lane — a freshly-built frontend served override-first over a stable backend. state is durable and
            observed: a push is <span className="mono">live</span> only when the edge is read back serving exactly that build.
          </div>
        </div>
      </div>

      {error && (
        <div className="empty mb-18" role="alert" style={{ color: "var(--bad-fg)" }}>
          {error}
        </div>
      )}

      <div className="kpis mb-20">
        <div className="card kpi"><div className="kpi-label">previews</div><div className="kpi-val">{counts.total}</div></div>
        <div className="card kpi"><div className="kpi-label">live</div><div className="kpi-val">{counts.live}</div></div>
        <div className="card kpi"><div className="kpi-label">pushed</div><div className="kpi-val">{counts.pushed}</div></div>
        <div className="card kpi"><div className="kpi-label">stale</div><div className="kpi-val">{counts.stale}</div></div>
        <div className="card kpi"><div className="kpi-label">ready</div><div className="kpi-val">{counts.provisioning}</div></div>
        <div className="card kpi"><div className="kpi-label">disabled</div><div className="kpi-val">{counts.disabled}</div></div>
        <div className="card kpi"><div className="kpi-label">error</div><div className="kpi-val">{counts.error}</div></div>
      </div>

      <div className="card mb-18">
        <div className="panel-head"><h3>Provision</h3></div>
        <div className="card-pad">
          {previewProjects.length === 0 ? (
            <div className="empty">No project enables the <span className="mono">live_preview</span> metadata key yet.</div>
          ) : (
            <form className="row gap-10" style={{ alignItems: "flex-end", flexWrap: "wrap" }} onSubmit={provision}>
              <label className="stack" style={{ gap: 4 }}>
                <span className="kpi-label">project</span>
                <select className="inp" value={pProject} onChange={(e) => setPProject(e.target.value)} required>
                  <option value="">select project</option>
                  {previewProjects.map((p) => (
                    <option key={p.name} value={p.name}>{p.name}</option>
                  ))}
                </select>
              </label>
              <label className="stack" style={{ gap: 4 }}>
                <span className="kpi-label">name <span className="dim">(optional)</span></span>
                <input className="inp" value={pName} onChange={(e) => setPName(e.target.value)} placeholder="defaults to session" />
              </label>
              <button
                type="submit"
                className="btn btn-sm btn-primary"
                disabled={!isAdmin || provisioning || !pProject}
                title={!isAdmin ? "admin only" : undefined}
              >
                {provisioning ? "provisioning…" : "provision"}
              </button>
            </form>
          )}
        </div>
      </div>

      {envs.length === 0 ? (
        <div className="empty">No preview environments yet.</div>
      ) : (
        <div className="grid-2">
          <div className="card">
            <div className="panel-head"><h3>Environments</h3><Pill tone={counts.stale ? "bad" : "ok"}>{envs.length} env{envs.length === 1 ? "" : "s"}</Pill></div>
            <table className="tbl">
              <thead>
                <tr><th>Preview</th><th>State</th><th>URL</th><th>Observed build</th><th>Observed</th></tr>
              </thead>
              <tbody>
                {envs.map((e) => (
                  <tr
                    key={key(e)}
                    className={`slot-row${selKey === key(e) ? " sel" : ""}`}
                    onClick={() => setSelKey(key(e))}
                  >
                    <td className="t-strong"><div className="row-title"><b>{e.name}</b><span className="mono dim">{e.project}</span></div></td>
                    <td><Pill tone={previewStateTone(e.state)} live={e.state === "live"}>{e.state}</Pill></td>
                    <td className="mono dim fs-sm" style={{ maxWidth: 200, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                      {e.url ? <a className="link" href={e.url} target="_blank" rel="noreferrer" onClick={(ev) => ev.stopPropagation()}>{e.url.replace(/^https?:\/\//, "")}</a> : "—"}
                    </td>
                    <td className="mono fs-sm">
                      {isTrustGap(e) ? (
                        <span className="trust-gap" title={e.detail || "pushed build not observed live"}>
                          <span className="dim strike">{shortBuild(e.live_build_id)}</span>
                          <span className="accent-bad"> not live</span>
                        </span>
                      ) : (
                        <span>{shortBuild(e.observed_build_id)}</span>
                      )}
                    </td>
                    <td className="mono dim fs-sm">{relTime(e.observed_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <div className="stack">
            {sel ? (
              <div className="card">
                <div className="panel-head">
                  <h3>{sel.name}</h3>
                  <Pill tone={previewStateTone(sel.state)} live={sel.state === "live"}>{sel.state}</Pill>
                </div>
                <div className="card-pad">
                  {isTrustGap(sel) && (
                    <div className="trust-banner" role="status">
                      <b>trust gap</b>
                      <span>pushed build <span className="mono">{shortBuild(sel.live_build_id)}</span> is not observed live. {sel.detail || "the edge is not serving the pushed build."}</span>
                    </div>
                  )}
                  <div className="grid-kv">
                    <div className="k">project</div><div className="v mono fs-sm">{sel.project}</div>
                    <div className="k">url</div>
                    <div className="v mono fs-sm" style={{ wordBreak: "break-all" }}>
                      {sel.url ? <a className="link" href={sel.url} target="_blank" rel="noreferrer">{sel.url}</a> : "—"}
                    </div>
                    <div className="k">claimed build</div>
                    <div className="v mono fs-sm">{shortBuild(sel.live_build_id)} <span className="dim">· {relTime(sel.pushed_at)}</span></div>
                    <div className="k">observed build</div>
                    <div className="v mono fs-sm">
                      {sel.observed_build_id ? <span className="accent">{shortBuild(sel.observed_build_id)}</span> : <span className="dim">none</span>}
                      <span className="dim"> · {relTime(sel.observed_at)}</span>
                    </div>
                    <div className="k">enabled</div><div className="v mono fs-sm">{sel.enabled ? "yes" : "no"}</div>
                    <div className="k">backend prefixes</div><div className="v mono fs-sm">{(sel.backend_prefixes ?? []).join(" ") || "—"}</div>
                    <div className="k">edge image</div><div className="v mono fs-sm" style={{ wordBreak: "break-all" }}>{sel.edge_image || "—"}</div>
                    <div className="k">subject</div><div className="v mono fs-sm" style={{ wordBreak: "break-all" }}>{sel.authorized_subject || "—"}</div>
                    <div className="k">session</div><div className="v mono fs-sm">{sel.session_id || "—"}</div>
                    <div className="k">updated</div><div className="v mono fs-sm">{relTime(sel.updated_at)}</div>
                    {sel.detail && !isTrustGap(sel) && (<><div className="k">detail</div><div className="v fs-sm" style={{ color: sel.state === "error" ? "var(--bad-fg)" : undefined }}>{sel.detail}</div></>)}
                  </div>

                  <div className="row gap-10 mt-16" style={{ flexWrap: "wrap" }}>
                    {sel.url && (
                      <a className="btn btn-sm" href={sel.url} target="_blank" rel="noreferrer">
                        <Icon name="ext" />open
                      </a>
                    )}
                    {sel.enabled ? (
                      confirm?.kind === "disable" && confirm.key === selKey ? (
                        <span className="confirm">
                          <span className="q">disable?</span>
                          <button className="btn btn-sm btn-danger" disabled={busy === `${selKey}:disable`} onClick={() => void control(sel, "disable")}>{busy === `${selKey}:disable` ? "…" : "Yes"}</button>
                          <button className="btn btn-sm" onClick={() => setConfirm(null)}>Keep</button>
                        </span>
                      ) : (
                        <button className="btn btn-sm" disabled={!isAdmin} title={!isAdmin ? "admin only" : undefined} onClick={() => setConfirm({ kind: "disable", key: selKey! })}>disable</button>
                      )
                    ) : (
                      <button className="btn btn-sm" disabled={!isAdmin || busy === `${selKey}:enable`} title={!isAdmin ? "admin only" : undefined} onClick={() => void control(sel, "enable")}>{busy === `${selKey}:enable` ? "enabling…" : "enable"}</button>
                    )}
                    {confirm?.kind === "delete" && confirm.key === selKey ? (
                      <span className="confirm">
                        <span className="q">delete?</span>
                        <button className="btn btn-sm btn-danger" disabled={busy === `${selKey}:delete`} onClick={() => void control(sel, "delete")}>{busy === `${selKey}:delete` ? "…" : "Yes"}</button>
                        <button className="btn btn-sm" onClick={() => setConfirm(null)}>Keep</button>
                      </span>
                    ) : (
                      <button className="btn btn-sm btn-danger" disabled={!isAdmin} title={!isAdmin ? "admin only" : undefined} onClick={() => setConfirm({ kind: "delete", key: selKey! })}>delete</button>
                    )}
                  </div>
                </div>
              </div>
            ) : (
              <div className="card"><div className="card-pad"><div className="empty">Select a preview to inspect.</div></div></div>
            )}
          </div>
        </div>
      )}
    </>
  );
}
