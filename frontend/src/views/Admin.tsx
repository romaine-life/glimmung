import { useState } from "react";
import { Icon } from "../ui/Icon";
import { Pill } from "../ui/bits";
import { authedFetch } from "../auth";
import { relTime, useJson, useLayout } from "./lib";

type Me = { signed_in?: boolean; email?: string; name?: string; is_admin?: boolean };

export function Admin() {
  const { snap, isAdmin } = useLayout();
  const me = useJson<Me>("/v1/auth/me");
  const projects = snap?.projects ?? [];
  const admissions = snap?.test_slot_admissions ?? [];
  const runtime = snap?.agent_runtime as unknown as Record<string, unknown> | undefined;

  const [name, setName] = useState("");
  const [repo, setRepo] = useState("");
  const [status, setStatus] = useState<"idle" | "saving" | "ok" | "error">("idle");
  const [message, setMessage] = useState<string | null>(null);

  const register = async () => {
    if (!name.trim() || !repo.trim()) return;
    setStatus("saving");
    setMessage(null);
    try {
      const r = await authedFetch("/v1/projects", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: name.trim(), github_repo: repo.trim() }),
      });
      if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
      setStatus("ok");
      setName("");
      setRepo("");
    } catch (e) {
      setStatus("error");
      setMessage(String(e));
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="display">Admin</h1>
          <div className="sub">Projects, agent runtime, identity, and the test-slot pool.</div>
        </div>
      </div>

      <div className="grid-2">
        <div className="stack">
          <div className="card">
            <div className="panel-head"><h3>Projects</h3><Pill tone="info">{projects.length}</Pill></div>
            <table className="tbl">
              <thead><tr><th>Project</th><th>Repo</th><th>Registered</th></tr></thead>
              <tbody>
                {projects.map((p) => (
                  <tr key={p.id}>
                    <td className="t-strong">{p.name}</td>
                    <td className="mono dim">{p.github_repo}</td>
                    <td className="dim">{relTime(p.created_at)}</td>
                  </tr>
                ))}
                {projects.length === 0 && <tr><td colSpan={3}><div className="empty">No projects registered.</div></td></tr>}
              </tbody>
            </table>
            <div className="card-pad" style={{ borderTop: "1px solid var(--border)" }}>
              <div className="eyebrow mb-10">Register project</div>
              <div className="grid-2col">
                <div className="field"><label>Name</label><input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="glimmung" /></div>
                <div className="field"><label>GitHub repo</label><input className="input mono" value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="romaine-life/glimmung" /></div>
              </div>
              <div className="row gap-10 mt-12">
                <button className="btn btn-primary" disabled={!isAdmin || status === "saving" || !name.trim() || !repo.trim()} onClick={() => void register()}>
                  <Icon name="plus" />{status === "saving" ? "registering…" : "Register"}
                </button>
                {status === "ok" && <Pill tone="ok">registered</Pill>}
                {status === "error" && <span className="pill bad" title={message ?? undefined}>error</span>}
                {!isAdmin && <span className="dim fs-sm">admin only</span>}
              </div>
            </div>
          </div>

          <div className="card">
            <div className="panel-head"><h3>Agent runtime</h3></div>
            <div className="card-pad">
              {runtime ? (
                <div className="grid-kv">
                  <div className="k">default slot</div><div className="v mono fs-sm">{String(runtime["default_slot"] ?? runtime["default"] ?? "—")}</div>
                  <div className="k">config</div><div className="v"><Pill tone="ok">loaded</Pill></div>
                </div>
              ) : (
                <div className="empty">No agent runtime config in snapshot.</div>
              )}
            </div>
          </div>
        </div>

        <div className="stack">
          <div className="card">
            <div className="panel-head"><h3>Identity</h3></div>
            <div className="card-pad">
              <div className="grid-kv">
                <div className="k">signed in</div><div className="v"><Pill tone={me.data?.signed_in ? "ok" : "neutral"}>{me.data?.signed_in ? "yes" : "no"}</Pill></div>
                <div className="k">name</div><div className="v fs-sm">{me.data?.name ?? "—"}</div>
                <div className="k">email</div><div className="v mono fs-sm">{me.data?.email ?? "—"}</div>
                <div className="k">admin</div><div className="v"><Pill tone={me.data?.is_admin ? "ok" : "neutral"}>{me.data?.is_admin ? "admin" : "member"}</Pill></div>
              </div>
            </div>
          </div>

          <div className="card">
            <div className="panel-head"><h3>Test-slot pool</h3></div>
            {admissions.length === 0 ? (
              <div className="card-pad"><div className="empty">No slot admissions reported.</div></div>
            ) : (
              <table className="tbl">
                <thead><tr><th>Project</th><th>Configured</th><th>Available</th><th>Claimed</th><th>Waiting</th></tr></thead>
                <tbody>
                  {admissions.map((a) => (
                    <tr key={a.project}>
                      <td className="t-strong">{a.project}</td>
                      <td className="mono dim">{a.configured_test_slots}</td>
                      <td className="mono">{a.checkout_available_test_slots}</td>
                      <td className="mono dim">{a.claimed_test_slots}</td>
                      <td>{a.waiting_checkout_requests ? <span className="mono" style={{ color: "var(--info-fg)" }}>{a.waiting_checkout_requests}</span> : <span className="dim">—</span>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
      </div>
    </>
  );
}
