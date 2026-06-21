import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter, Outlet, Route, Routes } from "react-router-dom";

import { Previews } from "./Previews";
import { previewStateTone } from "./lib";
import type { LayoutContext, PreviewEnvironment, Snapshot } from "../App";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function env(overrides: Partial<PreviewEnvironment>): PreviewEnvironment {
  return {
    project: "glimmung",
    name: "session-x",
    lease_ref: "preview-glimmung-session-x",
    session_id: "x",
    authorized_subject: "tank/session/x",
    enabled: true,
    state: "ready",
    url: "https://glimmung-session-x.preview.romaine.life",
    upstream_url: "http://127.0.0.1:8080",
    backend_prefixes: ["/v1"],
    image_tag: "glimmung:sha-abc",
    edge_image: "live-preview-edge:0.1",
    live_build_id: "",
    pushed_at: null,
    observed_build_id: "",
    observed_at: null,
    detail: "",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    ...overrides,
  };
}

function renderPreviews(snap: Snapshot | null, isAdmin = true) {
  const ctx: LayoutContext = { snap, signedIn: true, isAdmin, selected: { kind: "all" } };
  return render(
    <MemoryRouter initialEntries={["/previews"]}>
      <Routes>
        <Route element={<Outlet context={ctx} />}>
          <Route path="/previews" element={<Previews />} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

function snapshot(envs: PreviewEnvironment[]): Snapshot {
  return {
    active_leases: [],
    projects: [
      { id: "p-glimmung", name: "glimmung", github_repo: "romaine-life/glimmung", metadata: { live_preview: { enabled: true, backend_prefixes: ["/v1"] } }, created_at: new Date().toISOString() },
    ],
    workflows: [],
    preview_environments: envs,
  };
}

describe("previewStateTone", () => {
  it("maps every preview state into the allowed pill set {ok,warn,bad,info}", () => {
    const allowed = new Set(["ok", "warn", "bad", "info"]);
    for (const s of ["provisioning", "ready", "pushed", "live", "stale", "disabled", "error", "weird"]) {
      expect(allowed.has(previewStateTone(s))).toBe(true);
    }
  });

  it("renders the trust gap (stale) and failure (error) as the bad/drain tone", () => {
    expect(previewStateTone("stale")).toBe("bad");
    expect(previewStateTone("error")).toBe("bad");
  });

  it("renders observed-confirmed states (ready/live) as the free tone", () => {
    expect(previewStateTone("ready")).toBe("ok");
    expect(previewStateTone("live")).toBe("ok");
  });
});

describe("Previews view", () => {
  it("shows an explicit connecting state before the first snapshot", () => {
    renderPreviews(null);
    expect(screen.getByText("Connecting…")).toBeInTheDocument();
  });

  it("shows an explicit empty state when there are no previews", () => {
    renderPreviews(snapshot([]));
    expect(screen.getByText("No preview environments yet.")).toBeInTheDocument();
  });

  it("surfaces the stale trust gap distinctly and renders OBSERVED, not claimed, build", async () => {
    const stale = env({
      name: "session-stale",
      state: "stale",
      live_build_id: "build-CLAIMED",
      observed_build_id: "build-OBSERVED",
      detail: "edge serving build build-OBSERVED, expected build-CLAIMED",
    });
    renderPreviews(snapshot([stale]));

    // The state pill uses the bad/drain tone (allowed set), never an invented pill.
    const pill = screen.getAllByText("stale").find((el) => el.className.includes("pill"));
    expect(pill?.className).toContain("bad");

    // The row does not present the claimed build as if it were live.
    expect(screen.getByText(/not live/i)).toBeInTheDocument();

    // Pinning the row reveals the explicit trust-gap banner.
    await userEvent.click(screen.getByText("session-stale"));
    const banner = screen.getByText("trust gap").closest(".trust-banner") as HTMLElement;
    expect(banner).toBeInTheDocument();
    // The banner names the CLAIMED build that the edge is not observed serving.
    expect(within(banner).getAllByText(/build-CLAIMED/).length).toBeGreaterThan(0);
  });

  it("does not mark a pushed-but-unobserved env as live (observed build is none)", async () => {
    const pushed = env({ name: "session-pushed", state: "pushed", live_build_id: "build-CLAIMED", observed_build_id: "", observed_at: null });
    renderPreviews(snapshot([pushed]));
    await userEvent.click(screen.getByText("session-pushed"));
    const inspector = screen.getByText("observed build").closest(".grid-kv") as HTMLElement;
    expect(within(inspector).getByText("none")).toBeInTheDocument();
  });

  it("requires a second click to disable (two-step inline confirm) and does not call the API on the first", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }));
    renderPreviews(snapshot([env({ name: "session-on", state: "live", enabled: true })]));
    await userEvent.click(screen.getByText("session-on"));

    await userEvent.click(screen.getByRole("button", { name: "disable" }));
    // Arming the confirm must not fire the durable mutation.
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(screen.getByText("disable?")).toBeInTheDocument();
  });

  it("gates controls behind admin", async () => {
    renderPreviews(snapshot([env({ name: "session-on", state: "live" })]), false);
    await userEvent.click(screen.getByText("session-on"));
    expect(screen.getByRole("button", { name: "disable" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "delete" })).toBeDisabled();
  });
});
