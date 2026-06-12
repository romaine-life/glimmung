import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";

import {
  App,
  CONNECTION_DEAD_AFTER_MS,
  CONNECTION_STALE_AFTER_MS,
  buildBreadcrumbs,
  connectionStateFromSnapshotClock,
} from "./App";
import { installMockFetch, isMockMode } from "./mockApi";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  window.history.pushState({}, "", "/");
});

describe("connection status", () => {
  it("keeps transient SSE reconnects out of the dead state", () => {
    const startedAt = 10_000;
    const lastSeen = startedAt + 2_000;

    expect(connectionStateFromSnapshotClock(startedAt + 1_000, startedAt, 0)).toBe("stale");
    expect(connectionStateFromSnapshotClock(lastSeen + CONNECTION_STALE_AFTER_MS - 1, startedAt, lastSeen)).toBe("live");
    expect(connectionStateFromSnapshotClock(lastSeen + CONNECTION_STALE_AFTER_MS, startedAt, lastSeen)).toBe("stale");
    expect(connectionStateFromSnapshotClock(lastSeen + CONNECTION_DEAD_AFTER_MS, startedAt, lastSeen)).toBe("dead");
  });
});

describe("mock mode", () => {
  it("does not persist mock mode onto ordinary paths", () => {
    window.history.pushState({}, "", "/?mock=1");
    expect(isMockMode()).toBe(true);

    sessionStorage.setItem("glimmung.mock.enabled", "1");
    window.history.pushState({}, "", "/");

    expect(isMockMode()).toBe(false);
  });
});

describe("breadcrumbs", () => {
  it("tracks issue run selections down to phase, job, and step", () => {
    const jobCrumbs = buildBreadcrumbs(
      "/projects/ambience/issues/170/runs/3/cycles/3/phases/env-prep/jobs/env-prep",
    );
    expect(jobCrumbs.map((crumb) => crumb.label)).toEqual([
      "Home",
      "Projects",
      "ambience",
      "Issues",
      "#170",
      "Runs",
      "run 3",
      "cycle 3",
      "phase env-prep",
      "job env-prep",
    ]);

    const crumbs = buildBreadcrumbs(
      "/projects/ambience/issues/170/runs/3/cycles/3/phases/env-prep/jobs/env-prep/steps/clone-repo",
    );

    expect(crumbs.map((crumb) => crumb.label)).toEqual([
      "Home",
      "Projects",
      "ambience",
      "Issues",
      "#170",
      "Runs",
      "run 3",
      "cycle 3",
      "phase env-prep",
      "job env-prep",
      "step clone-repo",
    ]);
    expect(crumbs[7]).toEqual({
      label: "cycle 3",
      to: "/projects/ambience/issues/170/runs/3/cycles/3",
    });
    expect(crumbs[8]).toEqual({
      label: "phase env-prep",
      to: "/projects/ambience/issues/170/runs/3/cycles/3/phases/env-prep",
    });
    expect(crumbs[9]).toEqual({
      label: "job env-prep",
      to: "/projects/ambience/issues/170/runs/3/cycles/3/phases/env-prep/jobs/env-prep",
    });
  });

  it("tracks issue settings", () => {
    const crumbs = buildBreadcrumbs("/projects/ambience/issues/170/settings");
    expect(crumbs.map((crumb) => crumb.label)).toEqual([
      "Home",
      "Projects",
      "ambience",
      "Issues",
      "#170",
      "Settings",
    ]);
  });
});

describe("test environment slots", () => {
  it("links a slot row to its inspectable detail page", async () => {
    window.history.pushState({}, "", "/projects/glimmung/leases/test?mock=1");
    installMockFetch();

    render(
      <MemoryRouter initialEntries={["/projects/glimmung/leases/test?mock=1"]}>
        <App />
      </MemoryRouter>,
    );

    const slotLink = await screen.findByRole("link", { name: "glimmung-test-1" });
    expect(slotLink).toHaveAttribute("href", "/projects/glimmung/leases/test/slots/1");

    await userEvent.click(slotLink);

    expect(await screen.findByRole("heading", { name: "glimmung-test-1" })).toBeInTheDocument();
    expect(screen.getByText("Raw slot snapshot")).toBeInTheDocument();
    expect(screen.getAllByText("glimmung/glimmung-test-1/leases/42").length).toBeGreaterThan(0);
  });
});

describe("project workflow definitions", () => {
  it("opens declared job steps from the canonical workflow page", async () => {
    window.history.pushState({}, "", "/projects/glimmung/workflows/default?mock=1");
    installMockFetch();

    render(
      <MemoryRouter
        initialEntries={[{
          pathname: "/projects/glimmung/workflows/default",
          search: "?mock=1",
          state: { returnTo: "/projects/glimmung/issues/206/settings", returnLabel: "issue #206" },
        }]}
      >
        <App />
      </MemoryRouter>,
    );

    expect(await screen.findByRole("link", { name: "← back to issue #206" })).toHaveAttribute(
      "href",
      "/projects/glimmung/issues/206/settings",
    );

    const jobLabel = await screen.findByText("PR touchpoint", { selector: ".dag-job-title" });
    expect(screen.queryByText("Ensure PR touchpoint")).not.toBeInTheDocument();
    expect(screen.getAllByText("1 planned step").length).toBeGreaterThan(0);

    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing workflow job button");
    await userEvent.click(jobButton);

    expect(await screen.findByText("native job inspector")).toBeInTheDocument();
    expect(screen.getByText("planned")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Ensure PR touchpoint/ })).toBeInTheDocument();
    expect(screen.getByText(/\$ step ensure-pr-touchpoint/)).toBeInTheDocument();
  });
});

describe("project runs", () => {
  it("cancels an active issue run from the project runs table", async () => {
    const requests: Array<{ path: string; method: string }> = [];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      const method = (init?.method ?? "GET").toUpperCase();
      requests.push({ path: url.pathname, method });
      if (url.pathname === "/v1/auth/me") {
        return new Response(JSON.stringify({
          signed_in: true,
          email: "admin@glimmung.test",
          name: "Admin",
          is_admin: true,
        }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      if (
        url.pathname === "/v1/projects/glimmung/issues/206/runs/1.1/abort" &&
        method === "POST"
      ) {
        return new Response(JSON.stringify({ ok: true }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      throw new Error(`unhandled fetch ${method} ${url.pathname}`);
    }));

    window.history.pushState({}, "", "/projects/glimmung/runs?mock=1");
    render(
      <MemoryRouter initialEntries={["/projects/glimmung/runs?mock=1"]}>
        <App />
      </MemoryRouter>,
    );

    expect((await screen.findAllByText("Duration")).length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(/24m \d+s elapsed/)).toBeInTheDocument();
    expect(screen.getAllByText("ran 1h 10m").length).toBeGreaterThanOrEqual(1);

    const cancelButtons = await screen.findAllByRole("button", { name: "cancel run" });
    expect(cancelButtons.length).toBeGreaterThan(1);
    expect(cancelButtons.some((button) => button.hasAttribute("disabled"))).toBe(true);

    const activeCancel = cancelButtons.find((button) => !button.hasAttribute("disabled"));
    expect(activeCancel).toBeTruthy();
    await userEvent.click(activeCancel!);
    await userEvent.click(screen.getByRole("button", { name: "cancel?" }));

    await waitFor(() => {
      expect(requests).toContainEqual({
        path: "/v1/projects/glimmung/issues/206/runs/1.1/abort",
        method: "POST",
      });
    });
  });
});

describe("overview KPI click redirection", () => {
  it("routes clicking 'test' active leases KPI text to /leases with tab=test state", async () => {
    window.history.pushState({}, "", "/?mock=1");
    installMockFetch();

    const { container } = render(
      <MemoryRouter initialEntries={["/"]}>
        <App />
      </MemoryRouter>,
    );

    // Find the link wrapping the test leases text inside the first KPI card
    const kpis = container.querySelectorAll(".kpis .card.kpi");
    const leasesKpi = kpis[0];
    expect(leasesKpi).toBeDefined();
    
    // Find the test link inside the foot
    const testLink = leasesKpi.querySelector(".kpi-foot a");
    expect(testLink).not.toBeNull();
    expect(testLink?.textContent).toContain("test");

    await userEvent.click(testLink!);

    // It should navigate to /leases with "test" tab selected/displayed
    await waitFor(() => {
      const heading = container.querySelector("h1.display");
      expect(heading).toBeInTheDocument();
      expect(heading?.textContent).toBe("Leases");
    });

    // The "test" tab should have class "on"
    const testTab = container.querySelector(".tabs .tab.on");
    expect(testTab).toBeInTheDocument();
    expect(testTab?.textContent).toBe("test");
  });
});
