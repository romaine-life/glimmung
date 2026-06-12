import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RecyclePolicyPanel, type RecyclePolicyWorkflow } from "./RecyclePolicyPanel";

const workflow: RecyclePolicyWorkflow = {
  project: "ambience",
  name: "default",
  phases: [
    {
      name: "verify",
      recycle_policy: { max_attempts: 3, on: ["verify_fail"], lands_at: "implement" },
    },
    { name: "implement", recycle_policy: null },
  ],
  pr: {
    recycle_policy: {
      max_attempts: 2,
      on: ["pr_review_changes_requested"],
      lands_at: "implement",
    },
  },
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("RecyclePolicyPanel", () => {
  it("shows lane counts read-only when not admin", () => {
    render(<RecyclePolicyPanel workflow={workflow} signedIn={false} isAdmin={false} />);

    expect(screen.getByText("verify")).toBeInTheDocument();
    expect(screen.getByText("pr reject")).toBeInTheDocument();
    // counts shown as text, no inputs
    expect(screen.queryByRole("spinbutton")).not.toBeInTheDocument();
    expect(screen.getByText("admin sign-in required to scale")).toBeInTheDocument();
    // only phases with a recycle policy plus the pr lane render as rows
    // (implement has no policy); body rows = verify + pr reject = 2.
    const rows = screen.getAllByRole("row");
    // 1 header row + 2 lane rows
    expect(rows).toHaveLength(3);
  });

  it("lets an admin scale a lane and PATCHes the workflow", async () => {
    let patchBody: unknown = null;
    let patchUrl: string | null = null;
    const onSaved = vi.fn();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        // authedFetch resolves auth config (/v1/config) before the real call;
        // only capture the workflow PATCH.
        if (init?.method === "PATCH") {
          patchUrl = url;
          patchBody = init?.body ? JSON.parse(String(init.body)) : null;
          return new Response(null, { status: 204 });
        }
        return new Response(JSON.stringify({}), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    );

    render(
      <RecyclePolicyPanel
        workflow={workflow}
        signedIn
        isAdmin
        onSaved={onSaved}
      />,
    );

    const verifyInput = screen.getByLabelText("verify max attempts") as HTMLInputElement;
    expect(verifyInput.value).toBe("3");

    await userEvent.clear(verifyInput);
    await userEvent.type(verifyInput, "5");

    const apply = screen.getByRole("button", { name: "apply" });
    expect(apply).toBeEnabled();
    await userEvent.click(apply);

    expect(patchUrl).toBe("/v1/workflows/ambience/default");
    expect(patchBody).toEqual({
      recycle_max_attempts: [{ target: "verify", max_attempts: 5 }],
    });
    expect(onSaved).toHaveBeenCalled();
  });

  it("renders a pinned lane read-only with the pinned pill, even for admins", () => {
    const pinned: RecyclePolicyWorkflow = {
      ...workflow,
      control_pins: {
        "phases.verify.recycle_policy": {
          pinned_by: "operator@romaine.life",
          reason: "systemic fails must not recycle",
        },
      },
    };
    render(<RecyclePolicyPanel workflow={pinned} signedIn isAdmin />);

    expect(screen.getByText("pinned")).toBeInTheDocument();
    // pinned lane has no scale input; the unpinned pr lane still does
    expect(screen.queryByLabelText("verify max attempts")).not.toBeInTheDocument();
    expect(screen.getByLabelText("pr reject max attempts")).toBeInTheDocument();
    // pinned lane offers unpin, unpinned lanes offer pin
    expect(screen.getByRole("button", { name: "unpin" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "pin" })).toBeInTheDocument();
  });

  it("pins a lane with a reason via PUT after the inline flow", async () => {
    let putUrl: string | null = null;
    let putBody: unknown = null;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (init?.method === "PUT") {
          putUrl = url;
          putBody = init?.body ? JSON.parse(String(init.body)) : null;
          return new Response(JSON.stringify({}), { status: 200 });
        }
        return new Response(JSON.stringify({}), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    );

    render(<RecyclePolicyPanel workflow={workflow} signedIn isAdmin />);

    // open the pin flow on the verify lane (first pin button)
    await userEvent.click(screen.getAllByRole("button", { name: "pin" })[0]);
    const reasonInput = screen.getByLabelText("verify pin reason");
    await userEvent.type(reasonInput, "gate stays at 1");
    const row = reasonInput.closest("tr");
    if (!row) throw new Error("pin flow row not found");
    await userEvent.click(within(row as HTMLElement).getByRole("button", { name: "pin" }));

    expect(putUrl).toBe(
      "/v1/workflows/ambience/default/control-pins/phases.verify.recycle_policy",
    );
    expect(putBody).toEqual({ reason: "gate stays at 1" });
  });

  it("unpins via two-step confirm with DELETE", async () => {
    let deleteUrl: string | null = null;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (init?.method === "DELETE") {
          deleteUrl = url;
          return new Response(JSON.stringify({}), { status: 200 });
        }
        return new Response(JSON.stringify({}), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }),
    );

    const pinned: RecyclePolicyWorkflow = {
      ...workflow,
      control_pins: { "pr.recycle_policy": { pinned_by: "operator@romaine.life" } },
    };
    render(<RecyclePolicyPanel workflow={pinned} signedIn isAdmin />);

    await userEvent.click(screen.getByRole("button", { name: "unpin" }));
    // step 1 swapped in the confirm; nothing sent yet
    expect(deleteUrl).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "unpin?" }));

    expect(deleteUrl).toBe("/v1/workflows/ambience/default/control-pins/pr.recycle_policy");
  });
});
