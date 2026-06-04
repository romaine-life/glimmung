import { useEffect, useState, type ReactNode } from "react";
import { NavLink, Link, useLocation } from "react-router-dom";
import { Icon } from "./Icon";
import { buildBreadcrumbs } from "../routes";

type NavItem = { key: string; label: string; icon: string; to: string; end?: boolean };
type NavGroup = { group: string; items: NavItem[] };

// Canonical IA — mirrors the redesign's sidebar groups. Counts are layered in
// from the live snapshot at render time (see `counts` below).
const NAV: NavGroup[] = [
  { group: "Work", items: [
    { key: "overview", label: "Overview", icon: "dashboard", to: "/", end: true },
    { key: "attention", label: "Needs attention", icon: "alert", to: "/needs-attention" },
    { key: "issues", label: "Issues", icon: "issue", to: "/issues" },
    { key: "runs", label: "Runs", icon: "runs", to: "/runs" },
  ]},
  { group: "Review", items: [
    { key: "touchpoints", label: "Touchpoints", icon: "pr", to: "/touchpoints" },
  ]},
  { group: "Orchestrate", items: [
    { key: "workflows", label: "Workflows", icon: "workflow", to: "/workflows" },
  ]},
  { group: "Capacity", items: [
    { key: "leases", label: "Leases", icon: "lease", to: "/leases" },
    { key: "slots", label: "Test slots", icon: "flask", to: "/test-slots" },
  ]},
  { group: "System", items: [
    { key: "admin", label: "Admin", icon: "settings", to: "/admin" },
  ]},
];

// Minimal shape Shell reads — kept local so Shell stays decoupled from App.tsx.
export type ShellSnapshot = {
  active_leases?: unknown[];
  test_environments?: unknown[];
  waiting_test_slot_requests?: unknown[];
  projects?: { name: string; github_repo?: string }[];
  inflight_locks?: { issues?: boolean; prs?: boolean };
} | null;

export type ShellAccount = {
  signedIn: boolean;
  name?: string | null;
  email?: string | null;
  avatarUrl?: string | null;
  isAdmin?: boolean;
} | null;

export type ShellConnection = "live" | "stale" | "dead" | "connecting";

function initials(account: ShellAccount): string {
  const src = account?.name || account?.email || "";
  const parts = src.replace(/@.*$/, "").split(/[.\s_-]+/).filter(Boolean);
  if (parts.length === 0) return "··";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

function ProfileAvatar({ account }: { account: ShellAccount }) {
  const avatarUrl = account?.avatarUrl?.trim() ?? "";
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    setFailed(false);
  }, [avatarUrl]);

  if (!avatarUrl || failed) {
    return <div className="avatar" aria-hidden="true">{initials(account)}</div>;
  }

  return (
    <div className="avatar avatar-image" aria-hidden="true">
      <img src={avatarUrl} alt="" referrerPolicy="no-referrer" onError={() => setFailed(true)} />
    </div>
  );
}

export function Shell({
  snap,
  account,
  connection = snap ? "live" : "connecting",
  isMock = false,
  onSignIn,
  onSignOut,
  onRefresh,
  children,
}: {
  snap: ShellSnapshot;
  account: ShellAccount;
  connection?: ShellConnection;
  isMock?: boolean;
  onSignIn?: () => void;
  onSignOut?: () => void;
  onRefresh?: () => void;
  children: ReactNode;
}) {
  const location = useLocation();
  const crumbs = buildBreadcrumbs(location.pathname);
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);

  const leaseCount = snap?.active_leases?.length ?? 0;
  const slotCount = snap?.test_environments?.length ?? 0;
  const waiting = snap?.waiting_test_slot_requests?.length ?? 0;
  const attentionAlert = Boolean(snap?.inflight_locks?.issues || snap?.inflight_locks?.prs);
  const counts: Record<string, { count?: number; alert?: boolean }> = {
    attention: { alert: attentionAlert },
    leases: { count: leaseCount || undefined },
    slots: { count: slotCount || undefined, alert: waiting > 0 },
  };

  const projects = snap?.projects ?? [];
  const project =
    projects.length === 1
      ? { name: projects[0].name, sub: projects[0].github_repo ?? "" }
      : { name: "All projects", sub: projects.length ? `${projects.length} projects` : "glimmung" };

  const connLabel = connection === "live" ? "live" : connection === "connecting" ? "connecting" : connection;
  const connClass = connection === "stale" ? "conn stale" : connection === "dead" ? "conn dead" : "conn";

  return (
    <div className={`app${sidebarCollapsed ? " sidebar-collapsed" : ""}`}>
      <aside className="sidebar" id="app-sidebar">
        <div className="brand">
          <div className="brand-mark" />
          <div className="wordmark">glimmung</div>
          <button
            className="sidebar-toggle"
            type="button"
            title={sidebarCollapsed ? "open sidebar" : "collapse sidebar"}
            aria-label={sidebarCollapsed ? "open sidebar" : "collapse sidebar"}
            aria-controls="app-sidebar"
            aria-expanded={!sidebarCollapsed}
            onClick={() => setSidebarCollapsed((current) => !current)}
          >
            <Icon name={sidebarCollapsed ? "chevright" : "chevleft"} className="ic sidebar-toggle-icon" />
          </button>
        </div>
        <Link className="project-switch" to="/projects">
          <div className="ps-label">
            <span className="ps-name">{project.name}</span>
            <span className="ps-sub">{project.sub}</span>
          </div>
          <Icon name="chevdown" className="ic chev" />
        </Link>
        <nav className="nav">
          {NAV.map((g) => (
            <div className="nav-group" key={g.group}>
              <div className="eyebrow">{g.group}</div>
              {g.items.map((it) => {
                const c = counts[it.key];
                return (
                  <NavLink
                    key={it.key}
                    to={it.to}
                    end={it.end}
                    className={({ isActive }) => `nav-item${isActive ? " active" : ""}`}
                  >
                    <Icon name={it.icon} />
                    <span className="nav-label">{it.label}</span>
                    {c?.count != null && (
                      <span className={`count${c.alert ? " alert" : ""}`}>{c.count}</span>
                    )}
                    {c?.count == null && c?.alert && (
                      <span className="count alert">•</span>
                    )}
                  </NavLink>
                );
              })}
            </div>
          ))}
        </nav>
        <div className="sidebar-foot">
          {account?.signedIn ? (
            <>
              <ProfileAvatar account={account} />
              <div className="who">
                <b>{account.name || account.email}</b>
                <span>{account.isAdmin ? "admin" : "member"}</span>
              </div>
              <button
                className="btn btn-ghost btn-sm mla"
                title="sign out"
                onClick={onSignOut}
              >
                <Icon name="x" />
              </button>
            </>
          ) : (
            <>
              <div className={`${connClass}`}><span className="dot" />{connLabel}</div>
              <button className="btn btn-sm btn-primary mla" onClick={onSignIn}>
                Sign in
              </button>
            </>
          )}
          {account?.signedIn && (
            <div className={`${connClass}`} style={{ marginLeft: 8 }}>
              <span className="dot" />
              {connLabel}
            </div>
          )}
        </div>
      </aside>

      <div className="main">
        <div className="topbar">
          <div className="crumbs">
            {crumbs.map((c, i) => {
              const last = i === crumbs.length - 1;
              return (
                <span key={`${c.label}:${i}`} className="row gap-8">
                  {i > 0 && <span className="sep">/</span>}
                  {last ? <b>{c.label}</b> : c.to ? <Link to={c.to}>{c.label}</Link> : <span>{c.label}</span>}
                </span>
              );
            })}
          </div>
          <div className="topbar-spacer" />
          {isMock && <span className="pill info">mock</span>}
          <button className="btn btn-ghost btn-sm" onClick={onRefresh ?? (() => window.location.reload())}>
            <Icon name="refresh" />
            Refresh
          </button>
        </div>
        <div className="content">{children}</div>
      </div>
    </div>
  );
}
