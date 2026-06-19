import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter, Outlet, Route, Routes, useLocation } from "react-router-dom";

import { IssueDetailView, pickDecisionReview, agentTranscriptEntries, runnerTerminalText, type RunnerEvent } from "./IssueDetailView";
import { ISSUE_DETAIL_CHILD_ROUTES } from "./routes";

describe("pickDecisionReview", () => {
  const tp = (runRef: string, pr: number, state = "open") => ({
    ref: `tp-${pr}`,
    repo: "romaine-life/ambience",
    pr_number: pr,
    title: `PR ${pr}`,
    state,
    linked_run_ref: runRef,
    evidence: [],
  });

  it("prefers the issue's current run review over the first parked one", () => {
    const stale = tp("ambience#168/runs/25.1", 290);
    const current = tp("ambience#168/runs/28.3", 297);
    expect(
      pickDecisionReview([stale, current], "ambience#168/runs/28.3")?.pr_number,
    ).toBe(297);
  });

  it("falls back to the first review needing a decision when the current run has none", () => {
    const mergedCurrent = tp("ambience#168/runs/28.3", 297, "merged");
    const parked = tp("ambience#168/runs/27.1", 296);
    expect(
      pickDecisionReview([mergedCurrent, parked], "ambience#168/runs/28.3")?.pr_number,
    ).toBe(296);
  });
});

const issueDetail = {
  ref: "ambience#172",
  project: "ambience",
  repo: "romaine-life/ambience",
  number: 172,
  title: "Effect: Distant storm at sea horizon",
  body: "storm",
  state: "open",
  labels: ["ambient-effects"],
  html_url: null,
  metadata: {},
  comments: [],
  last_run_ref: "ambience#172/runs/7.1",
  last_run_number: 7,
  last_run_state: "in_progress",
  issue_lock_held: true,
};

const runProjection = {
  issue_ref: "ambience#172",
  current_run_ref: "ambience#172/runs/7.1",
  default_focus: { kind: "run", ref: "ambience#172/runs/7.1" },
  next_action: { kind: "watch_run", label: "watch run", target_ref: "ambience#172/runs/7.1" },
  reviews: [],
  signals: [],
  edges: [],
  runs: [{
    run_ref: "ambience#172/runs/7.1",
    run_number: 7,
    run_display_number: "7.1",
    cycle_number: 7,
    run_cycle_number: 1,
    workflow: "default",
    state: "in_progress",
    current_phase: "env-prep",
    validation_url: null,
    cost_usd: 0,
    attempts_count: 1,
    started_at: "2026-05-20T17:24:09.336Z",
    updated_at: "2026-05-20T17:24:09.696Z",
    completed_at: null,
    evidence: [],
    topology: {
      phases: [{
        name: "env-prep",
        kind: "k8s_job",
        verify: false,
        run_on: "success",
        purpose: "work",
        depends_on: [],
        jobs: [{ id: "env-prep", name: "Environment prep" }],
	      }, {
	        name: "agent-execute",
	        kind: "k8s_job",
	        verify: false,
	        run_on: "success",
	        purpose: "work",
	        depends_on: ["env-prep"],
	        jobs: [{ id: "agent", name: "Run agent" }],
	      }, {
	        name: "review",
	        kind: "k8s_job",
	        verify: false,
	        run_on: "success",
	        purpose: "review",
	        depends_on: ["agent-execute"],
	        jobs: [{ id: "pr-review", name: "PR review" }],
	      }],
	      default_entry: { target: "env-prep", active: true, kind: "default" },
	      recycle_arrows: [{
	        source: "review",
        target: "env-prep",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "review_recycle",
      }],
    },
    phases: [{
      name: "env-prep",
      kind: "k8s_job",
      state: "active",
      verify: false,
      run_on: "success",
      purpose: "work",
      depends_on: [],
      jobs: [{
        id: "env-prep",
        name: "Environment prep",
        state: "active",
        started_at: "2026-05-20T17:24:09.336Z",
        k8s_job_name: "glim-ambience-172-runs-7-1-0-env-prep",
        steps: [
          {
            slug: "clone-repo",
            title: "Clone repository",
            state: "active",
            started_at: "2026-05-20T17:24:10.000Z",
          },
          { slug: "build-validation-image", title: "Build validation image", state: "not_started" },
        ],
      }],
      attempts: [{
        attempt_index: 0,
        state: "dispatching",
        conclusion: null,
        verification_status: null,
        decision: null,
        log_archive_url: null,
        evidence_refs: [],
        job_completions: [],
      }],
    }, {
      name: "agent-execute",
      kind: "k8s_job",
      state: "not_started",
      verify: false,
      run_on: "success",
      purpose: "work",
      depends_on: ["env-prep"],
	      jobs: [{
	        id: "agent",
	        name: "Run agent",
	        state: "not_started",
        steps: [
          { slug: "checkout", title: "Checkout workspace", state: "not_started" },
          { slug: "run-agent", title: "Run agent", state: "not_started" },
        ],
	      }],
	      attempts: [],
	    }, {
	      name: "review",
	      kind: "k8s_job",
	      state: "not_started",
	      verify: false,
	      run_on: "success",
	      purpose: "review",
	      depends_on: ["agent-execute"],
	      jobs: [{
	        id: "pr-review",
	        name: "PR review",
	        state: "not_started",
	        steps: [
	          { slug: "ensure-pr-review", title: "Ensure PR review", state: "not_started" },
	        ],
	      }],
	      attempts: [],
	    }],
  }],
};

const issueGraph = {
  issue_ref: "ambience#172",
  nodes: [
    {
      id: "issue:ambience#172",
      kind: "issue",
      label: "#172 Effect: Distant storm at sea horizon",
      state: "open",
      timestamp: null,
      metadata: { project: "ambience", number: 172 },
    },
    {
      id: "run:ambience#172/runs/7.1",
      kind: "run",
      label: "Run 7.1",
      state: "in_progress",
      timestamp: "2026-05-20T17:24:09.336Z",
      metadata: {
        run_number: 7,
        run_display_number: "7.1",
        cycle_number: 7,
        run_cycle_number: 1,
        workflow: "default",
      },
    },
  ],
  edges: [
    { source: "issue:ambience#172", target: "run:ambience#172/runs/7.1", kind: "spawned" },
  ],
  projection: runProjection,
};

const agentWorkflow = {
  id: "workflow:ambience/default",
  project: "ambience",
  name: "default",
  workflow_filename: null,
  workflow_ref: null,
  default_requirements: {},
  pr: { recycle_policy: null },
  phases: [{
    name: "implementation",
    kind: "k8s_job",
    workflow_filename: "",
    workflow_ref: "",
    verify: false,
    recycle_policy: null,
    jobs: [{
      id: "agent",
      steps: [{ slug: "run-agent", type: "agent", agent: { slot: "implementation" } }],
    }],
  }, {
    name: "verification",
    kind: "k8s_job",
    workflow_filename: "",
    workflow_ref: "",
    verify: true,
    recycle_policy: null,
    jobs: [{
      id: "verify",
      steps: [{ slug: "verify-agent", type: "agent", agent: { slot: "verification" } }],
    }],
  }],
};

const runtimeContext = {
  signedIn: true,
  isAdmin: true,
  snap: {
    agent_runtime: {
      profiles: {
        "codex-deep": { id: "codex-deep", provider: "codex", model: "gpt-5.5", reasoning_effort: "high" },
        "claude-sonnet": { id: "claude-sonnet", provider: "claude", model: "claude-sonnet-4-5" },
      },
      policy: { default: { mode: "override", profile: "codex-deep" } },
    },
    projects: [{
      name: "ambience",
      github_repo: "romaine-life/ambience",
      metadata: {
        agent_runtime: {
          policy: { default: { mode: "inherit" } },
        },
      },
    }],
    workflows: [agentWorkflow],
  },
};

const runnerEvents = {
  project: "ambience",
  run_ref: "ambience#172/runs/7.1",
  attempt_index: 0,
  job_id: "env-prep",
  archive_url: null,
  events: [
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "env-prep",
      job_id: "env-prep",
      seq: 1,
      event: "log",
      step_slug: "clone-repo",
      message: "cloning repo",
      exit_code: null,
      metadata: {},
      created_at: "2026-05-20T17:24:10.000Z",
    },
  ],
};

const agentRunnerEvents = {
  project: "ambience",
  run_ref: "ambience#172/runs/7.1",
  attempt_index: 0,
  job_id: "agent",
  archive_url: null,
  events: [
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 1,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({ type: "system", subtype: "init", cwd: "/workspace" }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:10.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 2,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({
        type: "assistant",
        message: {
          content: [
            { type: "text", text: "I will inspect the file." },
            { type: "tool_use", id: "toolu_1", name: "Read", input: { file_path: "src/App.tsx" } },
          ],
        },
      }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:11.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 3,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({
        type: "user",
        message: {
          content: [{
            type: "tool_result",
            tool_use_id: "toolu_1",
            content: JSON.stringify({ stdout: "line one\nline two" }),
          }],
        },
      }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:12.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 4,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({
        type: "assistant",
        message: {
          content: [
            { type: "thinking", signature: "very-large-signature" },
            { type: "text", text: "Done." },
          ],
        },
      }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:13.000Z",
    },
    {
      project: "ambience",
      run_ref: "ambience#172/runs/7.1",
      attempt_index: 0,
      phase: "agent-execute",
      job_id: "agent",
      seq: 5,
      event: "log",
      step_slug: "run-agent",
      message: JSON.stringify({ type: "result", subtype: "success", duration_ms: 1250, total_cost_usd: 0.0123 }),
      exit_code: null,
      metadata: { stream: "stdout" },
      created_at: "2026-05-20T17:24:14.000Z",
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("IssueDetailView run execution graph", () => {
  it("keeps issue labels inline with the issue title", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/review");

    const heading = await screen.findByRole("heading", { name: issueDetail.title });
    const titleRow = heading.closest(".issue-title-row");
    if (!titleRow) throw new Error("missing issue title row");

    const labels = within(titleRow as HTMLElement).getByLabelText("issue labels");
    expect(within(labels).getByText("ambient-effects")).toBeInTheDocument();
    expect(within(labels).getByText("in flight")).toBeInTheDocument();
    expect(document.querySelector(".project-hero > .dag-policy-rail")).not.toBeInTheDocument();
    expect(document.querySelector(".issue-hero .project-facts")).not.toBeInTheDocument();
  });

  it("shows a human approval queued status without leaking the signal ref", async () => {
    const reviewIssue = {
      ...issueDetail,
      last_run_state: "review_required",
      issue_lock_held: false,
    };
    const reviewGraph = {
      ...issueGraph,
      nodes: [
        ...issueGraph.nodes,
        {
          id: "pr:review-ambience-172",
          kind: "pr" as const,
          label: "PR #327",
          state: "open",
          timestamp: null,
          metadata: {
            repo: "romaine-life/ambience",
            number: 327,
            title: "Review PR",
            html_url: "https://github.com/romaine-life/ambience/pull/327",
          },
        },
      ],
      projection: {
        ...runProjection,
        current_run_ref: "ambience#172/runs/7.1",
        runs: [{
          ...runProjection.runs[0],
          state: "review_required",
          current_phase: "review_gate",
          validation_url: "https://validation.test",
        }],
        reviews: [{
          ref: "review-ambience-172",
          repo: "romaine-life/ambience",
          pr_number: 327,
          title: "Review PR",
          state: "open",
          html_url: "https://github.com/romaine-life/ambience/pull/327",
          linked_run_ref: "ambience#172/runs/7.1",
          validation_url: "https://validation.test",
          evidence: [],
        }],
        signals: [],
      },
    };
    const signalBodies: Array<Record<string, unknown>> = [];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/config") return json({ auth_url: "https://auth.test", tank_operator_base_url: "https://tank.test" });
      if (url.pathname === "/v1/auth/me") return json({ signed_in: true, email: "admin@example.com" });
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(reviewIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(reviewGraph);
      if (url.pathname === "/v1/workflows") return json([agentWorkflow]);
      if (url.pathname === "/v1/signals" && init?.method === "POST") {
        signalBodies.push(init.body ? JSON.parse(String(init.body)) : {});
        return json({ ref: "signal:pr-327-approval", state: "pending" });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/review");

    await userEvent.click(await screen.findByRole("button", { name: "approve" }));

    expect(await screen.findByText("approval queued")).toBeInTheDocument();
    expect(screen.queryByText(/signal:p/)).not.toBeInTheDocument();
    expect(signalBodies[0]?.payload).toEqual({ kind: "approve" });
  });

  it("renders issue descriptions and comments as markdown", async () => {
    const markdownIssue = {
      ...issueDetail,
      body: [
        "## Concept",
        "",
        "Read clearly as `slimes` on grass.",
        "",
        "- [ ] Define config",
        "- hop on event",
      ].join("\n"),
      comments: [{
        id: "comment-1",
        author: "nelsong6",
        body: "Please see **rendered** notes.",
        created_at: "2026-05-20T17:24:09.336Z",
        updated_at: "2026-05-20T17:24:09.336Z",
      }],
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(markdownIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/summary");

    expect(await screen.findByRole("heading", { name: "Concept", level: 2 })).toBeInTheDocument();
    expect(screen.queryByText("## Concept")).not.toBeInTheDocument();
    expect(screen.getByText("slimes").tagName).toBe("CODE");
    const taskItem = screen.getByText("Define config").closest("li");
    expect(taskItem?.querySelector('input[type="checkbox"]')).toBeTruthy();
    expect(screen.getByText("rendered").tagName).toBe("STRONG");
  });

  it("edits issue agent runtime from the settings tab, not the summary editor", async () => {
    let patchBody: Record<string, unknown> | null = null;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172" && init?.method === "PATCH") {
        patchBody = JSON.parse(String(init.body)) as Record<string, unknown>;
        return json({ ...issueDetail, metadata: { agent: patchBody.agent } });
      }
      if (url.pathname === "/v1/config") return json({ auth_url: "https://auth.test", tank_operator_base_url: "https://tank.test" });
      if (url.pathname === "/v1/auth/me") return json({ signed_in: true, email: "admin@example.com" });
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([agentWorkflow]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/summary", runtimeContext);

    await userEvent.click(await screen.findByRole("button", { name: "edit" }));
    expect(screen.queryByText("Agent runtime")).not.toBeInTheDocument();

    expect(screen.queryByRole("button", { name: "workflow" })).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "settings" }));
    expect(await screen.findByRole("heading", { name: "Issue agent runtime" })).toBeInTheDocument();
    expect(screen.getByText("new runs only")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "view workflow definition" })).toHaveAttribute(
      "href",
      "/projects/ambience/workflows/default",
    );

    await userEvent.selectOptions(screen.getByLabelText("Agent runtime"), "claude-sonnet");
    await userEvent.selectOptions(screen.getByLabelText("verification agent"), "codex-deep");
    await userEvent.click(screen.getByRole("button", { name: "Save agent runtime" }));

    await waitFor(() => {
      expect(patchBody).toEqual({
        agent: {
          default: { mode: "override", profile: "claude-sonnet" },
          slots: {
            implementation: { mode: "inherit" },
            verification: { mode: "override", profile: "codex-deep" },
          },
        },
      });
    });
  });

  it("edits issue labels from the settings tab and patches only labels", async () => {
    let patchBody: Record<string, unknown> | null = null;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172" && init?.method === "PATCH") {
        patchBody = JSON.parse(String(init.body)) as Record<string, unknown>;
        return json({ ...issueDetail, labels: (patchBody.labels as string[]) ?? [] });
      }
      if (url.pathname === "/v1/config") return json({ auth_url: "https://auth.test", tank_operator_base_url: "https://tank.test" });
      if (url.pathname === "/v1/auth/me") return json({ signed_in: true, email: "admin@example.com" });
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([agentWorkflow]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings", runtimeContext);

    // The labels editor lives on the settings tab, not the summary editor.
    expect(await screen.findByRole("heading", { name: "Issue labels" })).toBeInTheDocument();

    // Seeded chip mirrors the issue's existing label and carries an inline remove.
    const chips = screen.getByLabelText("issue label chips");
    expect(within(chips).getByText("ambient-effects")).toBeInTheDocument();

    // Drop the seeded label and stage a fresh one via the add input.
    await userEvent.click(
      within(chips).getByRole("button", { name: "remove label ambient-effects" }),
    );
    await userEvent.type(screen.getByLabelText("add label"), "needs-spec");
    await userEvent.keyboard("{Enter}");

    await userEvent.click(screen.getByRole("button", { name: "Save labels" }));

    // Only labels are sent — title/body/state are left untouched by this PATCH.
    await waitFor(() => {
      expect(patchBody).toEqual({ labels: ["needs-spec"] });
    });
  });

  it("folds an uncommitted label draft into the save", async () => {
    let patchBody: Record<string, unknown> | null = null;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172" && init?.method === "PATCH") {
        patchBody = JSON.parse(String(init.body)) as Record<string, unknown>;
        return json({ ...issueDetail, labels: (patchBody.labels as string[]) ?? [] });
      }
      if (url.pathname === "/v1/config") return json({ auth_url: "https://auth.test", tank_operator_base_url: "https://tank.test" });
      if (url.pathname === "/v1/auth/me") return json({ signed_in: true, email: "admin@example.com" });
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([agentWorkflow]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings", runtimeContext);

    await screen.findByRole("heading", { name: "Issue labels" });
    // Type a label but do not press enter — save must still include it.
    await userEvent.type(screen.getByLabelText("add label"), "backend");
    await userEvent.click(screen.getByRole("button", { name: "Save labels" }));

    await waitFor(() => {
      expect(patchBody).toEqual({ labels: ["ambient-effects", "backend"] });
    });
  });

  it("clears all labels and patches an empty array", async () => {
    let patchBody: Record<string, unknown> | null = null;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172" && init?.method === "PATCH") {
        patchBody = JSON.parse(String(init.body)) as Record<string, unknown>;
        return json({ ...issueDetail, labels: (patchBody.labels as string[]) ?? [] });
      }
      if (url.pathname === "/v1/config") return json({ auth_url: "https://auth.test", tank_operator_base_url: "https://tank.test" });
      if (url.pathname === "/v1/auth/me") return json({ signed_in: true, email: "admin@example.com" });
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([agentWorkflow]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings", runtimeContext);

    await screen.findByRole("heading", { name: "Issue labels" });
    const chips = screen.getByLabelText("issue label chips");
    // Remove the only label; the editor falls back to its empty state.
    await userEvent.click(
      within(chips).getByRole("button", { name: "remove label ambient-effects" }),
    );
    expect(within(chips).getByText("no labels yet")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Save labels" }));

    await waitFor(() => {
      expect(patchBody).toEqual({ labels: [] });
    });
  });

  it("renders declared dispatch inputs as a form and sends entered values on dispatch", async () => {
    const unlockedIssue = {
      ...issueDetail,
      issue_lock_held: false,
      last_run_state: "passed",
    };
    const workflowWithInputs = {
      ...agentWorkflow,
      dispatch_inputs: [
        {
          name: "git_ref",
          description: "branch or sha to check out",
          required: true,
          default: "main",
        },
      ],
    };
    let dispatchBody: unknown = null;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/config") return json({ auth_url: "https://auth.test", tank_operator_base_url: "https://tank.test" });
      if (url.pathname === "/v1/auth/me") return json({ signed_in: true, email: "admin@example.com" });
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(unlockedIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([workflowWithInputs]);
      if (url.pathname === "/v1/runs/dispatch" && init?.method === "POST") {
        dispatchBody = init.body ? JSON.parse(String(init.body)) : null;
        return json({
          state: "dispatched",
          issue_ref: "ambience#172",
          issue_number: 172,
          run_number: 8,
          cycle_number: 8,
          run_cycle_number: 1,
          run_ref: "ambience#172/runs/8.1",
        });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    const contextWithInputsWorkflow = {
      ...runtimeContext,
      snap: {
        ...(runtimeContext.snap ?? {}),
        workflows: [workflowWithInputs],
      },
    };
    renderIssueDetail("/projects/ambience/issues/172/settings", contextWithInputsWorkflow);

    expect(await screen.findByRole("heading", { name: "New run" })).toBeInTheDocument();
    // Default is rendered as the controlled value; user types over it.
    const gitRefInput = await screen.findByLabelText(/git_ref \*/);
    expect((gitRefInput as HTMLInputElement).value).toBe("main");
    await userEvent.clear(gitRefInput);
    await userEvent.type(gitRefInput, "feature/lanterns");
    await userEvent.click(screen.getByRole("button", { name: "dispatch" }));

    await waitFor(() => {
      expect(dispatchBody).toBeTruthy();
    });
    const payload = dispatchBody as { inputs?: Record<string, string> };
    expect(payload.inputs).toEqual({ git_ref: "feature/lanterns" });
  });

  it("routes new run from runs to issue settings", async () => {
    const unlockedIssue = {
      ...issueDetail,
      issue_lock_held: false,
      last_run_state: "passed",
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(unlockedIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs");

    await userEvent.click(await screen.findByRole("button", { name: "new run" }));

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/settings",
      );
    });
    expect(await screen.findByRole("heading", { name: "New run" })).toBeInTheDocument();
  });

  it("opens the newly dispatched run after dispatching from settings", async () => {
    const unlockedIssue = {
      ...issueDetail,
      issue_lock_held: false,
      last_run_state: "passed",
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(unlockedIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/runs/dispatch" && init?.method === "POST") {
        return json({
          state: "dispatched",
          issue_ref: "ambience#172",
          issue_number: 172,
          run_number: 8,
          cycle_number: 8,
          run_cycle_number: 1,
          run_ref: "ambience#172/runs/8.1",
        });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings");

    await userEvent.click(await screen.findByRole("button", { name: "dispatch" }));

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/8/cycles/1",
      );
    });
  });

  it("opens a queued run when dispatch finds no capacity", async () => {
    const unlockedIssue = {
      ...issueDetail,
      issue_lock_held: false,
      last_run_state: "passed",
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(unlockedIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/runs/dispatch" && init?.method === "POST") {
        return json({
          state: "queued",
          issue_ref: "ambience#172",
          issue_number: 172,
          run_number: 8,
          cycle_number: 8,
          run_cycle_number: 1,
          run_ref: "ambience#172/runs/8.1",
          detail: "queued awaiting project test slot capacity",
        });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings");

    await userEvent.click(await screen.findByRole("button", { name: "dispatch" }));

    // A queued run is a real, durable run — dispatch lands on it so the user
    // sees it waiting for capacity instead of the click silently settling.
    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/8/cycles/1",
      );
    });
  });

  it("surfaces a no-run dispatch outcome instead of silently settling", async () => {
    const unlockedIssue = {
      ...issueDetail,
      issue_lock_held: false,
      last_run_state: "passed",
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(unlockedIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/runs/dispatch" && init?.method === "POST") {
        return json({
          state: "already_running",
          issue_ref: "ambience#172",
          issue_number: 172,
          run_number: null,
          workflow: "default",
          detail: 'issue ambience#172 already has a non-terminal run (state "in_progress"); not dispatching a duplicate',
        });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings");

    await userEvent.click(await screen.findByRole("button", { name: "dispatch" }));

    // The reason is shown in place; the view does not navigate away to a run
    // that was never created.
    expect(await screen.findByText(/already has a non-terminal run/)).toBeInTheDocument();
    expect(screen.getByTestId("path")).toHaveTextContent(
      "/projects/ambience/issues/172/settings",
    );
  });

  it("shows run history as flat run counts, base cycle values, and run-cycle ordinals", async () => {
    const baseRun = runProjection.runs[0];
    const historyRuns = [
      {
        ...baseRun,
        run_ref: "ambience#172/runs/1.1",
        run_number: 1,
        run_display_number: "1.1",
        cycle_number: 1,
        run_cycle_number: 1,
        state: "recycled",
        started_at: "2026-05-20T17:24:09.336Z",
      },
      {
        ...baseRun,
        run_ref: "ambience#172/runs/2.1",
        run_number: 2,
        run_display_number: "2.1",
        cycle_number: 2,
        run_cycle_number: 1,
        state: "recycled",
        started_at: "2026-05-20T18:24:09.336Z",
      },
      {
        ...baseRun,
        run_ref: "ambience#172/runs/2.2",
        run_number: 2,
        run_display_number: "2.2",
        cycle_number: 3,
        run_cycle_number: 2,
        state: "in_progress",
        started_at: "2026-05-20T19:24:09.336Z",
      },
    ];
    const historyProjection = {
      ...runProjection,
      current_run_ref: "ambience#172/runs/2.2",
      default_focus: { kind: "run", ref: "ambience#172/runs/2.2" },
      next_action: { kind: "watch_run", label: "watch run", target_ref: "ambience#172/runs/2.2" },
      runs: historyRuns,
    };
    const historyGraph = {
      ...issueGraph,
      nodes: [
        issueGraph.nodes[0],
        ...historyRuns.map((run) => ({
          id: `run:${run.run_ref}`,
          kind: "run",
          label: `Run ${run.run_display_number}`,
          state: run.state,
          timestamp: run.started_at,
          metadata: {
            run_number: run.run_number,
            run_display_number: run.run_display_number,
            cycle_number: run.cycle_number,
            run_cycle_number: run.run_cycle_number,
            workflow: run.workflow,
          },
        })),
      ],
      edges: [
        { source: "run:ambience#172/runs/2.1", target: "run:ambience#172/runs/2.2", kind: "cycled_from" },
      ],
      projection: historyProjection,
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(historyGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/2/cycles/2/graph") {
        return json({ ...historyProjection, runs: [historyRuns[2]] });
      }
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs");

    const table = await screen.findByRole("table");
    const rows = within(table).getAllByRole("row");
    const newestCells = within(rows[1]).getAllByRole("cell");
    const middleCells = within(rows[2]).getAllByRole("cell");
    const oldestCells = within(rows[3]).getAllByRole("cell");

    expect(within(rows[0]).getByText("Duration")).toBeInTheDocument();
    expect(newestCells[0]).toHaveTextContent(/^3$/);
    expect(within(newestCells[1]).getByRole("button")).toHaveTextContent(/^2$/);
    expect(newestCells[1]).not.toHaveTextContent(/cycle/i);
    expect(newestCells[1]).not.toHaveTextContent(/\./);
    expect(newestCells[2]).toHaveTextContent(/^2$/);
    expect(newestCells[5]).toHaveTextContent(/elapsed$/);

    expect(middleCells[0]).toHaveTextContent(/^2$/);
    expect(within(middleCells[1]).getByRole("button")).toHaveTextContent(/^2$/);
    expect(middleCells[2]).toHaveTextContent(/^1$/);

    expect(oldestCells[0]).toHaveTextContent(/^1$/);
    expect(within(oldestCells[1]).getByRole("button")).toHaveTextContent(/^1$/);

    await userEvent.click(within(middleCells[3]).getByRole("button", { name: /recycled/i }));
    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/2/cycles/2",
      );
    });
  });

  it("keeps cancel run visible but disabled for a terminal issue run", async () => {
    const terminalIssue = {
      ...issueDetail,
      last_run_state: "aborted",
      issue_lock_held: false,
    };
    const terminalRun = {
      ...runProjection.runs[0],
      state: "aborted",
      completed_at: "2026-05-20T17:30:09.336Z",
    };
    const terminalProjection = {
      ...runProjection,
      runs: [terminalRun],
    };
    const terminalGraph = {
      ...issueGraph,
      nodes: issueGraph.nodes.map((node) => node.kind === "run" ? { ...node, state: "aborted" } : node),
      projection: terminalProjection,
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(terminalIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(terminalGraph);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs");

    const cancel = await screen.findByRole("button", { name: "cancel run" });
    expect(cancel).toBeDisabled();
    expect(cancel).toHaveAttribute("title", "run already aborted");
  });

  it("keeps cancel run visible but disabled on a deep-linked terminal execution view", async () => {
    const terminalIssue = {
      ...issueDetail,
      last_run_state: "aborted",
      issue_lock_held: false,
    };
    const terminalRun = {
      ...runProjection.runs[0],
      state: "aborted",
      completed_at: "2026-05-20T17:30:09.336Z",
    };
    const terminalProjection = {
      ...runProjection,
      runs: [terminalRun],
    };
    const terminalGraph = {
      ...issueGraph,
      nodes: issueGraph.nodes.map((node) => node.kind === "run" ? { ...node, state: "aborted" } : node),
      projection: terminalProjection,
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(terminalIssue);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(terminalGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(terminalProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const cancel = await screen.findByRole("button", { name: "cancel run" });
    expect(cancel).toBeDisabled();
    expect(cancel).toHaveAttribute("title", "run already aborted");
  });

  it("cancels an active run from a deep-linked execution view", async () => {
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
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/abort" && method === "POST") return json({ state: "aborted" });
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${method} ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const cancel = await screen.findByRole("button", { name: "cancel run" });
    expect(cancel).toBeEnabled();
    await userEvent.click(cancel);
    await userEvent.click(screen.getByRole("button", { name: "cancel?" }));

    await waitFor(() => {
      expect(requests).toContainEqual({
        path: "/v1/projects/ambience/issues/172/runs/7.1/abort",
        method: "POST",
      });
    });
  });

  it("routes a dispatching job click to its job path and keeps step clicks specific", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") return json(runnerEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const jobLabel = await screen.findByText("Environment prep");
    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing graph job button");
    expect(within(jobButton).queryByText("Build validation image")).not.toBeInTheDocument();
    await userEvent.click(jobButton);

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep/jobs/env-prep",
      );
    });
    expect(await screen.findByText("runner job inspector")).toBeInTheDocument();
    const runPanelMeta = document.querySelector(".run-panel-meta");
    expect(runPanelMeta).not.toBeNull();
    expect(within(runPanelMeta as HTMLElement).queryByText(/^attempt$/)).not.toBeInTheDocument();
    expect(within(runPanelMeta as HTMLElement).getByText(/^duration$/)).toBeInTheDocument();
    expect(within(runPanelMeta as HTMLElement).getByText(/elapsed$/)).toBeInTheDocument();
    expect(within(screen.getByLabelText("runner job steps")).getAllByText(/elapsed/).length).toBeGreaterThanOrEqual(2);
    expect(await screen.findByText("Click a step to see its logs.")).toBeInTheDocument();
    expect(screen.queryByText(/cloning repo/)).not.toBeInTheDocument();
    expect(within(screen.getByLabelText("runner job steps")).getByRole("button", { name: /Build validation image/ })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /Build validation image/ }));
    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep/jobs/env-prep/steps/build-validation-image",
      );
    });
  });

  it("renders an aborted step as the causal terminal node", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(abortedProjection());
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        return json({
          ...runnerEvents,
          events: [{
            ...runnerEvents.events[0],
            seq: 4,
            event: "step_aborted",
            step_slug: "probe-mod-set",
            message: "step \"probe-mod-set\" requested run abort: baselib_missing_or_unversioned",
            metadata: { abort_reason: "baselib_missing_or_unversioned" },
          }],
        });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep/jobs/env-prep/steps/probe-mod-set");

    expect(await screen.findByRole("button", { name: /Verify allowed mods/ })).toHaveTextContent("aborted");
    expect(await screen.findByText(/reason baselib_missing_or_unversioned/)).toBeInTheDocument();
  });

  it("renders the terminal verify cause in the inspector when a failed verify job is selected", async () => {
    // Regression for the spirelens#147 bug: selecting the failed verify job
    // replaced the run summary (the only place the cause rendered) with the
    // inspector, which showed only green steps and "Click a step to see its
    // logs". The SPECIFIC cause — typed terminal_observation, abort_reason,
    // and the deciding attempt's expected/observed verification failure — must
    // travel into the inspector with the selection. Fails on the pre-fix code
    // (no .run-failure-cause in the inspector); passes once the cause renders.
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") {
        return json(verifierFailedProjection());
      }
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        return json({ ...runnerEvents, events: [] });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail(
      "/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/verify",
    );

    // The terminal-failure cause traveled into the inspector (not the run
    // summary) with the selection — rendered in the .run-failure-cause block.
    const causeEl = await waitFor(() => {
      const node = document.querySelector(".run-failure-cause");
      if (!node) throw new Error("cause block not rendered");
      return node as HTMLElement;
    });
    // The inspector — not the run summary — is showing the selected verify job.
    const runPanel = document.querySelector(".run-panel") as HTMLElement;
    expect(within(runPanel.querySelector(".run-panel-header") as HTMLElement).getByText("Verify effect"))
      .toBeInTheDocument();
    // Typed terminal_observation message + class.
    expect(
      within(causeEl).getByText(/expected "CLOAK_CLASP", observed "Cloak Clasp"/),
    ).toBeInTheDocument();
    expect(within(causeEl).getByText(/verifier_failed phase=llm-verify/)).toBeInTheDocument();
    // abort_reason.
    expect(within(causeEl).getByText("verification_failed")).toBeInTheDocument();
    // Deciding attempt's structured failure block (reused from AttemptCard).
    expect(within(causeEl).getByText("CLOAK_CLASP")).toBeInTheDocument();
    expect(within(causeEl).getByText(/Cloak Clasp \[effect label\]/)).toBeInTheDocument();
    // verification.reasons list.
    expect(
      within(causeEl).getByText("claimed_result_not_observed", { selector: "li" }),
    ).toBeInTheDocument();

    // The failed owner step is visible — not just green steps.
    const stepRail = await screen.findByLabelText("runner job steps");
    expect(within(stepRail).getByRole("button", { name: /Verdict/ })).toHaveTextContent("failed");
    expect(within(stepRail).getByRole("button", { name: /Run verification/ })).toHaveTextContent("exit 0");
  });

  it("selecting the synthesized verdict step surfaces the failure, not the no-events dead-end", async () => {
    // The exact spirelens#146 bug the user hit: clicking the `verdict` owner
    // step — which has no hot runner events by construction — rendered
    // "No hot runner events recorded for this selection." with only the bare
    // `verification_failed` enum. The pane must instead surface the deciding
    // verifier reason, the structured failure, and the attempt's summary prose.
    // Fails on pre-fix code (dead-end string + dropped message/summary).
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") {
        return json(verifierFailedProjection());
      }
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        // No hot events for the verdict selection — the dead-end trigger.
        return json({ ...runnerEvents, events: [] });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail(
      "/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/verify/steps/verdict",
    );

    // The verdict step pane rendered the structured failure detail.
    const verdictPane = await screen.findByLabelText("verdict detail");
    // The dead-end string is gone from the entire view for this selection.
    expect(screen.queryByText(/No hot runner events recorded/)).toBeNull();
    // The deciding verifier reason + structured failure are in the pane.
    expect(within(verdictPane).getByText("claimed_result_not_observed", { selector: "li" })).toBeInTheDocument();
    expect(within(verdictPane).getByText("CLOAK_CLASP")).toBeInTheDocument();
    // The rich summary prose — present ONLY on the verdict view, never in the
    // cause banner — proves the dropped summary_markdown now reaches the pane.
    expect(within(verdictPane).getByText(/the stat was not observed/)).toBeInTheDocument();
  });

  it("the terminal class list matches the canonical set", () => {
    // Tripwire mirroring the Go TestAllTerminalObservationClassesInventoryIsExact:
    // assert the canonical TS list equals an independent hardcoded set with no
    // duplicates. Adding a class to ALL_TERMINAL_OBSERVATION_CLASSES (because a
    // class was added to the Go AllTerminalObservationClasses) without adding it
    // to this expected set — or vice-versa — fails CI.
    const expected = [
      "producer_phase_failed",
      "verifier_contract_missing",
      "verifier_failed",
      "gate_failed",
      "dispatch_failed",
      "phase_requested_abort",
      "manual_abort",
      "malformed_terminal",
    ];
    expect([...ALL_TERMINAL_OBSERVATION_CLASSES].sort()).toEqual([...expected].sort());
    expect(new Set(ALL_TERMINAL_OBSERVATION_CLASSES).size).toBe(ALL_TERMINAL_OBSERVATION_CLASSES.length);
  });

  it.each(ALL_TERMINAL_OBSERVATION_CLASSES)(
    "renders a non-blank inspector cause for terminal class %s",
    async (terminalClass) => {
      // Enum-driven render inventory: for EVERY canonical terminal class, the
      // class-agnostic `.run-failure-cause` block must surface the class when the
      // failed verify job is selected. This guards a future per-class regression
      // where a new class would render the inspector blank (the spirelens#147 bug
      // class). It reuses slice 3's verifierFailedProjection fixture.
      vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
        const url =
          typeof input === "string"
            ? new URL(input, "https://glimmung.test")
            : input instanceof URL
              ? input
              : new URL(input.url);
        if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
        if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
        if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") {
          return json(verifierFailedProjectionForClass(terminalClass));
        }
        if (url.pathname === "/v1/workflows") return json([]);
        if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
          return json({ ...runnerEvents, events: [] });
        }
        throw new Error(`unhandled fetch ${url.pathname}`);
      }));

      // Deep-link to the synthesized verdict step: this guards BOTH the cause
      // banner AND the step pane a human clicks. The pane must never be the
      // "no events" dead-end for any terminal class.
      renderIssueDetail(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/verify/steps/verdict",
      );

      const causeEl = await waitFor(() => {
        const node = document.querySelector(".run-failure-cause");
        if (!node) throw new Error("cause block not rendered");
        return node as HTMLElement;
      });
      // The class travels into the cause display line (terminalObservationDisplay)
      // — proving this class does not render blank.
      expect(
        within(causeEl).getByText(new RegExp(`${terminalClass} phase=llm-verify`)),
      ).toBeInTheDocument();
      // The typed message is surfaced too.
      expect(
        within(causeEl).getByText(`terminal failure attributed to ${terminalClass}`),
      ).toBeInTheDocument();
      // The verdict step pane is rendered and is NEVER the no-events dead-end —
      // the frontend half of the no-dead-end invariant, enforced per class.
      const verdictPane = await screen.findByLabelText("verdict detail");
      expect(within(verdictPane).getByText("claimed_result_not_observed", { selector: "li" })).toBeInTheDocument();
      expect(screen.queryByText(/No hot runner events recorded/)).toBeNull();
    },
  );

  it("routes a phase header click to its phase breadcrumb path", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") return json(runnerEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const phaseTitle = await screen.findByText("Env prep", { selector: ".dag-phase-title" });
    const phaseButton = phaseTitle.closest("button");
    if (!phaseButton) throw new Error("missing phase header button");
    await userEvent.click(phaseButton);

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/env-prep",
      );
    });
    expect(await screen.findByText("runner job inspector")).toBeInTheDocument();
  });

  it("surfaces completed job cost in the selected job log section", async () => {
    const selectedProjection = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        phases: runProjection.runs[0].phases.map((phase) => phase.name === "env-prep"
          ? {
              ...phase,
              jobs: phase.jobs.map((job) => job.id === "env-prep"
                ? {
                    ...job,
                    state: "succeeded",
                    started_at: "2026-05-20T17:24:09.336Z",
                    completed_at: "2026-05-20T17:30:09.336Z",
                    cost_usd: 2.3456,
                  }
                : job),
            }
          : phase),
      }],
    };
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(selectedProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") return json(runnerEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const jobLabel = await screen.findByText("Environment prep");
    expect(screen.queryByText("$2.3456", { selector: ".dag-node-cost" })).not.toBeInTheDocument();
    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing graph job button");
    await userEvent.click(jobButton);

    expect(await screen.findByText("job cost")).toBeInTheDocument();
    expect(screen.getAllByText("$2.3456").length).toBeGreaterThanOrEqual(2);
    expect(within(screen.getByLabelText("runner job steps")).getByText("ran 6m 0s")).toBeInTheDocument();
  });

  it("links selected dynamic verification case evidence from the step detail", async () => {
    const evidenceProjection = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        evidence: [
          {
            kind: "screenshot",
            ref: "screenshots/dev-paper-lanterns-default-frame.png",
            label: "dev paper lanterns default frame",
            url: "/v1/artifacts/runs/ambience/run-7/screenshots/dev-paper-lanterns-default-frame.png",
            source_phase: "llm-verify",
            source_attempt_index: 2,
          },
          {
            kind: "video",
            ref: "videos/dev-paper-lanterns-default.webm",
            label: "dev paper lanterns default",
            url: "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-default.webm",
            duration_ms: 9100,
            source_phase: "llm-verify",
            source_attempt_index: 2,
          },
          {
            kind: "screenshot",
            ref: "screenshots/dev-paper-lanterns-release-pulse-frame.png",
            label: "dev paper lanterns release pulse decoded frame",
            url: "/v1/artifacts/runs/ambience/run-7/screenshots/dev-paper-lanterns-release-pulse-frame.png",
            source_phase: "llm-verify",
            source_attempt_index: 2,
          },
          {
            kind: "video",
            ref: "videos/dev-paper-lanterns-release-pulse.webm",
            label: "dev paper lanterns release pulse",
            url: "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-release-pulse.webm",
            duration_ms: 10400,
            source_phase: "llm-verify",
            source_attempt_index: 2,
          },
        ],
        phases: [
          ...runProjection.runs[0].phases,
          {
            name: "llm-verify",
            kind: "k8s_job",
            state: "failed",
            verify: true,
            run_on: "success",
            purpose: "work",
            depends_on: ["agent-execute"],
            jobs: [{
              id: "llm-verify",
              name: "LLM: Verify dynamic cases",
              state: "failed",
              reason: "verification_failed",
              steps: [
                { slug: "run-verification-case-03", title: "run-verification-case-03", state: "succeeded", group: "test-cases/case-03", group_title: "dev-paper-lanterns-release-pulse" },
                { slug: "emit-case-03", title: "emit-case-03", state: "failed", reason: "exit_nonzero", exit_code: 1, group: "test-cases/case-03", group_title: "dev-paper-lanterns-release-pulse" },
              ],
            }],
            attempts: [{
              attempt_index: 2,
              state: "failed",
              conclusion: "failure",
              verification_status: "fail",
              decision: "retry",
              log_archive_url: null,
              evidence_refs: [
                "screenshots/dev-paper-lanterns-release-pulse-frame.png",
                "videos/dev-paper-lanterns-release-pulse.webm",
              ],
              job_completions: [],
            }],
          },
        ],
      }],
    };
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(evidenceProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        return json({ ...runnerEvents, attempt_index: 2, job_id: "llm-verify", events: [] });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/llm-verify/steps/emit-case-03");

    const evidenceStrip = await screen.findByLabelText("step evidence");
    expect(evidenceStrip.closest(".runner-log-content")).toBeTruthy();
    expect(evidenceStrip.querySelector("video")).toHaveAttribute(
      "src",
      "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-release-pulse.webm",
    );
    expect(evidenceStrip.querySelector("video")?.closest(".runner-evidence-item")).toHaveClass("failed");
    expect(within(evidenceStrip).getByRole("img", { name: "dev paper lanterns release pulse decoded frame" })).toHaveAttribute(
      "src",
      "/v1/artifacts/runs/ambience/run-7/screenshots/dev-paper-lanterns-release-pulse-frame.png",
    );
    expect(within(evidenceStrip).getAllByTitle("verified evidence failed").length).toBeGreaterThanOrEqual(1);
    expect(within(evidenceStrip).getByRole("link", { name: "video: dev paper lanterns release pulse 10s" })).toHaveAttribute(
      "href",
      "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-release-pulse.webm",
    );
    expect(within(evidenceStrip).getByRole("link", { name: "frame: dev paper lanterns release pulse decoded frame" })).toHaveAttribute(
      "href",
      "/v1/artifacts/runs/ambience/run-7/screenshots/dev-paper-lanterns-release-pulse-frame.png",
    );
    expect(screen.queryByText("dev paper lanterns default frame")).not.toBeInTheDocument();
    expect(screen.queryByText("video: dev paper lanterns default 9s")).not.toBeInTheDocument();
  });

  it("shows collected job evidence in the job detail pane", async () => {
    const evidenceProjection = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        evidence: [
          {
            kind: "screenshot",
            ref: "screenshots/dev-paper-lanterns-default-frame.png",
            label: "dev paper lanterns default frame",
            url: "/v1/artifacts/runs/ambience/run-7/screenshots/dev-paper-lanterns-default-frame.png",
            source_phase: "llm-verify",
            source_attempt_index: 2,
          },
          {
            kind: "video",
            ref: "videos/dev-paper-lanterns-release-pulse.webm",
            label: "dev paper lanterns release pulse",
            url: "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-release-pulse.webm",
            duration_ms: 10400,
            source_phase: "llm-verify",
            source_attempt_index: 2,
          },
        ],
        phases: [
          ...runProjection.runs[0].phases,
          {
            name: "llm-verify",
            kind: "k8s_job",
            state: "failed",
            verify: true,
            run_on: "success",
            purpose: "work",
            depends_on: ["agent-execute"],
            jobs: [{
              id: "llm-verify",
              name: "LLM: Verify dynamic cases",
              state: "failed",
              reason: "verification_failed",
              steps: [
                { slug: "run-verification-case-01", title: "run-verification-case-01", state: "succeeded", group: "test-cases/case-01", group_title: "dev-paper-lanterns-default" },
                { slug: "emit-case-03", title: "emit-case-03", state: "failed", reason: "exit_nonzero", exit_code: 1, group: "test-cases/case-03", group_title: "dev-paper-lanterns-release-pulse" },
              ],
            }],
            attempts: [{
              attempt_index: 2,
              state: "failed",
              conclusion: "failure",
              verification_status: "fail",
              decision: "retry",
              log_archive_url: null,
              evidence_refs: [
                "screenshots/dev-paper-lanterns-default-frame.png",
                "videos/dev-paper-lanterns-release-pulse.webm",
              ],
              job_completions: [],
            }],
          },
        ],
      }],
    };
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(evidenceProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        return json({ ...runnerEvents, attempt_index: 2, job_id: "llm-verify", events: [] });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/llm-verify");

    expect(await screen.findByText("Click a step to see its logs.")).toBeInTheDocument();
    const logContent = document.querySelector(".runner-log-content");
    expect(logContent).not.toBeNull();
    expect(within(logContent as HTMLElement).queryByLabelText("step evidence")).not.toBeInTheDocument();

    const collected = await screen.findByLabelText("collected test evidence");
    expect(collected.closest(".runner-log-content")).toBe(logContent);
    expect(within(collected).getByText("Collected test evidence:")).toBeInTheDocument();
    expect(within(collected).getByRole("img", { name: "dev paper lanterns default frame" })).toHaveAttribute(
      "src",
      "/v1/artifacts/runs/ambience/run-7/screenshots/dev-paper-lanterns-default-frame.png",
    );
    expect(within(collected).getByRole("img", { name: "dev paper lanterns default frame" }).closest(".runner-evidence-item")).toHaveClass("passed");
    expect(within(collected).getByRole("link", { name: "video: dev paper lanterns release pulse 10s" })).toHaveAttribute(
      "href",
      "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-release-pulse.webm",
    );
    expect(within(collected).getByRole("link", { name: "video: dev paper lanterns release pulse 10s" }).closest(".runner-evidence-item")).toHaveClass("failed");

    await userEvent.click(screen.getByRole("button", { name: "view evidence for dev-paper-lanterns-release-pulse" }));

    expect(screen.getByTestId("path")).toHaveTextContent(
      "/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/llm-verify",
    );
    const testSetEvidence = await screen.findByLabelText("test set evidence");
    expect(testSetEvidence.closest(".runner-log-content")).toBe(logContent);
    expect(within(logContent as HTMLElement).getByText("Click a step to see its logs.")).toBeInTheDocument();
    expect(within(testSetEvidence).getByRole("link", { name: "video: dev paper lanterns release pulse 10s" })).toHaveAttribute(
      "href",
      "/v1/artifacts/runs/ambience/run-7/videos/dev-paper-lanterns-release-pulse.webm",
    );
    expect(within(testSetEvidence).getByRole("link", { name: "video: dev paper lanterns release pulse 10s" }).closest(".runner-evidence-item")).toHaveClass("failed");
    expect(within(testSetEvidence).queryByText("frame: dev paper lanterns default frame")).not.toBeInTheDocument();
    expect(within(screen.getByLabelText("runner job steps")).getByRole("button", { name: /emit-case-03/ })).toBeInTheDocument();
  });

  it("summarizes spawned verification agents without stacking long job names", async () => {
    const projectionWithInnerJobs = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        phases: [
          ...runProjection.runs[0].phases,
          {
            name: "llm-verify",
            kind: "k8s_job",
            state: "failed",
            verify: true,
            run_on: "success",
            purpose: "work",
            depends_on: ["agent-execute"],
            jobs: [{
              id: "llm-verify",
              name: "LLM: Verify dynamic cases",
              state: "failed",
              reason: "verification_failed",
              steps: [
                { slug: "run-verification-case-01", title: "run-verification-case-01", state: "succeeded", group: "test-cases/case-01", group_title: "dev-paper-lanterns-default" },
                { slug: "emit-case-01", title: "emit-case-01", state: "succeeded", exit_code: 0, group: "test-cases/case-01", group_title: "dev-paper-lanterns-default" },
                { slug: "run-verification-case-02", title: "run-verification-case-02", state: "succeeded", group: "test-cases/case-02", group_title: "dev-paper-lanterns-alt" },
                { slug: "emit-case-02", title: "emit-case-02", state: "succeeded", exit_code: 0, group: "test-cases/case-02", group_title: "dev-paper-lanterns-alt" },
                { slug: "run-verification-case-03", title: "run-verification-case-03", state: "succeeded", group: "test-cases/case-03", group_title: "dev-paper-lanterns-release-pulse" },
                { slug: "emit-case-03", title: "emit-case-03", state: "failed", reason: "exit_nonzero", exit_code: 1, group: "test-cases/case-03", group_title: "dev-paper-lanterns-release-pulse" },
              ],
            }],
            attempts: [{
              attempt_index: 2,
              state: "failed",
              conclusion: "failure",
              verification_status: "fail",
              decision: "retry",
              log_archive_url: null,
              evidence_refs: [],
              job_completions: [],
            }],
            inner_jobs: [
              { index: 1, completed_at: "2026-05-20T17:28:41.000Z" },
              { index: 2, completed_at: "2026-05-20T17:29:03.000Z" },
              { index: 3, completed_at: "2026-05-20T17:29:22.000Z" },
            ].map(({ index, completed_at }) => ({
              parent_job_id: "llm-verify",
              parent_step_slug: `run-verification-case-0${index}`,
              namespace: "ambience-slot-1",
              job_name: `agent-cb5d677f-78b1-4b3f-af3a-23745d4c33d9-vc${index}-2`,
              intent: "verification_agent",
              label: "verification",
              registered_at: "2026-05-20T17:24:10.000Z",
              completed_at,
              state: "succeeded",
              log_archive_url: `https://grafana.example.test/vc${index}`,
            })),
          },
        ],
      }],
    };
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(projectionWithInnerJobs);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        return json({ ...runnerEvents, attempt_index: 2, job_id: "llm-verify", events: [] });
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/llm-verify/jobs/llm-verify/steps/emit-case-03");

    expect(await screen.findByText("2 passed · 1 failed")).toBeInTheDocument();
    expect(screen.getByText(/longest vc3-2/)).toBeInTheDocument();
    expect(screen.queryByText(/vc1-2 ran/)).not.toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "verification agents" })).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "details" }));
    const table = screen.getByRole("table", { name: "verification agents" });
    expect(within(table).getByText("vc1-2")).toBeInTheDocument();
    expect(within(table).getByText("vc2-2")).toBeInTheDocument();
    expect(within(table).getByText("vc3-2")).toBeInTheDocument();
    expect(within(table).getAllByText("passed")).toHaveLength(2);
    expect(within(table).getByText("failed")).toBeInTheDocument();
    expect(within(table).getByText("case 01")).toBeInTheDocument();
    expect(within(table).getByText("case 03")).toBeInTheDocument();
    expect(within(table).getAllByRole("link", { name: "logs ↗" })).toHaveLength(3);
    expect(screen.queryByText("ambience-slot-1/agent-cb5d677f-78b1-4b3f-af3a-23745d4c33d9-vc1-2")).not.toBeInTheDocument();
  });

  it("renders LLM runner step JSON as a transcript while keeping raw logs available", async () => {
    const agentProjection = {
      ...runProjection,
      runs: [{
        ...runProjection.runs[0],
        current_phase: "agent-execute",
        phases: runProjection.runs[0].phases.map((phase) => {
          if (phase.name === "env-prep") {
            return {
              ...phase,
              state: "succeeded",
              jobs: phase.jobs.map((job) => ({
                ...job,
                state: "succeeded",
                steps: job.steps.map((step) => ({ ...step, state: "succeeded" })),
              })),
            };
          }
          if (phase.name === "agent-execute") {
            return {
              ...phase,
              state: "active",
              jobs: phase.jobs.map((job) => ({
                ...job,
                name: "llm agent",
                state: "active",
                steps: job.steps.map((step) => ({
                  ...step,
                  state: step.slug === "run-agent" ? "active" : "succeeded",
                })),
              })),
              attempts: [{
                attempt_index: 0,
                state: "active",
                conclusion: null,
                verification_status: null,
                decision: null,
                log_archive_url: null,
                evidence_refs: [],
                job_completions: [],
              }],
            };
          }
          return phase;
        }),
      }],
    };
    const agentGraph = { ...issueGraph, projection: agentProjection };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") return json(agentRunnerEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent");

    expect(await screen.findByLabelText("agent transcript")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "runner log view" })).toBeInTheDocument();
    expect(screen.getByText("I will inspect the file.")).toBeInTheDocument();
    expect(screen.getAllByText("Read").length).toBeGreaterThanOrEqual(1);
    const reasoningSummary = screen.getByText("reasoning signature").closest("summary");
    if (!reasoningSummary) throw new Error("missing reasoning signature summary");
    await userEvent.click(reasoningSummary);
    expect(screen.getByText(/No readable thinking text/)).toBeInTheDocument();
    expect(screen.getByText((content) => content.includes("very-large-signature"))).toBeInTheDocument();

    const toolResultSummary = screen.getByText("tool result").closest("summary");
    if (!toolResultSummary) throw new Error("missing tool result summary");
    await userEvent.click(toolResultSummary);
    expect(screen.getByText(/line two/)).toBeInTheDocument();

    await userEvent.click(within(screen.getByRole("group", { name: "transcript filter" })).getByRole("button", { name: "assistant" }));
    const filteredTranscript = screen.getByLabelText("agent transcript");
    expect(within(filteredTranscript).getByText("I will inspect the file.")).toBeInTheDocument();
    expect(within(filteredTranscript).getByText("Done.")).toBeInTheDocument();
    expect(within(filteredTranscript).queryByText("tool call")).not.toBeInTheDocument();
    expect(within(filteredTranscript).queryByText("tool result")).not.toBeInTheDocument();
    expect(within(filteredTranscript).queryByText("reasoning signature")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "raw" }));
    expect(screen.getByText((content) => content.includes("\"tool_use\""))).toBeInTheDocument();
    expect(screen.getByText((content) => content.includes("\\nline two"))).toBeInTheDocument();
  });

  it("keeps raw stdout fragments out of the LLM transcript", async () => {
    const agentProjection = activeAgentProjection();
    const agentGraph = { ...issueGraph, projection: agentProjection };
    const noisyAgentEvents = {
      ...agentRunnerEvents,
      events: [
        {
          ...agentRunnerEvents.events[0],
          seq: 0,
          message: "{",
          metadata: { stream: "stdout" },
        },
        {
          ...agentRunnerEvents.events[0],
          seq: 1,
          message: "  \"namespace\": \"ambience-slot-2\",",
          metadata: { stream: "stdout" },
        },
        ...agentRunnerEvents.events.map((event) => ({ ...event, seq: event.seq + 10 })),
      ],
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") return json(noisyAgentEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent");

    const transcript = await screen.findByLabelText("agent transcript");
    const firstEntry = transcript.querySelector(".agent-transcript-entry");
    expect(firstEntry).toHaveTextContent("assistant");
    expect(within(transcript).queryByText(/stdout log/i)).not.toBeInTheDocument();
    expect(within(transcript).queryByText(/system init/i)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "raw" }));
    expect(screen.getByText((content, element) => (
      element?.tagName === "PRE" && content.includes("{")
    ))).toBeInTheDocument();
  });

  it("keeps non-agent steps in an LLM job on the raw terminal view", async () => {
    const agentProjection = activeAgentProjection();
    const agentGraph = { ...issueGraph, projection: agentProjection };
    const checkoutRunnerEvents = {
      ...agentRunnerEvents,
      events: [{
        project: "ambience",
        run_ref: "ambience#172/runs/7.1",
        attempt_index: 0,
        phase: "agent-execute",
        job_id: "agent",
        seq: 1,
        event: "log",
        step_slug: "checkout",
        message: "{",
        exit_code: null,
        metadata: { stream: "stdout" },
        created_at: "2026-05-20T17:24:10.000Z",
      }],
    };

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") return json(checkoutRunnerEvents);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/checkout");

    expect(await screen.findByText((content, element) => (
      element?.tagName === "PRE" && content.includes("$ step checkout")
    ))).toBeInTheDocument();
    expect(screen.queryByLabelText("agent transcript")).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "runner log view" })).not.toBeInTheDocument();
    expect(screen.getByText((content, element) => (
      element?.tagName === "PRE" && content.includes("{")
    ))).toBeInTheDocument();
  });

  it("pages runner events in fixed batches without accumulating prior rows", async () => {
    const agentProjection = activeAgentProjection();
    const agentGraph = { ...issueGraph, projection: agentProjection };
    const firstPageEvents = {
      ...agentRunnerEvents,
      events: Array.from({ length: 200 }, (_, index) => {
        const seq = index + 1;
        return agentPageEvent(seq, [{
          type: "tool_use",
          id: `toolu_${seq}`,
          name: "Read",
          input: { seq },
        }]);
      }),
    };
    const secondPageEvents = {
      ...agentRunnerEvents,
      events: [
        agentPageEvent(201, [{ type: "text", text: "Readable line on the second batch." }]),
      ],
    };
    const eventSearches: string[] = [];

    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(agentGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(agentProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7.1/run/events") {
        eventSearches.push(url.search);
        return json(url.searchParams.get("after_seq") === "200" ? secondPageEvents : firstPageEvents);
      }
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent");

    expect(await screen.findByLabelText("agent transcript")).toBeInTheDocument();
    expect(screen.getByText(/200 events/)).toHaveTextContent(/batch 1/);
    expect(screen.queryByText("Readable line on the second batch.")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "next batch" }));

    expect(await screen.findByText("Readable line on the second batch.")).toBeInTheDocument();
    expect(screen.getByText(/1 event/)).toHaveTextContent(/batch 2/);
    expect(eventSearches.some((search) => search.includes("after_seq=200"))).toBe(true);

    await userEvent.click(screen.getByRole("button", { name: "previous batch" }));

    await waitFor(() => {
      expect(screen.queryByText("Readable line on the second batch.")).not.toBeInTheDocument();
    });
    expect(screen.getByText(/200 events/)).toHaveTextContent(/batch 1/);
  });

  it("omits the issue run rollup panel between the header and tabs", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    expect(await screen.findByLabelText("issue sections")).toBeInTheDocument();
    expect(screen.queryByLabelText("issue run rollup")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "cycle 7.1 execution" })).toBeInTheDocument();
  });

  it("opens planned steps for a job before any attempt has started", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/projects/ambience/issues/172/runs/7/cycles/1/graph") return json(runProjection);
      if (url.pathname === "/v1/workflows") return json([]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/runs/7/cycles/1");

    const jobLabel = await screen.findByText("Run agent", { selector: ".dag-job-title" });
    const jobButton = jobLabel.closest("button");
    if (!jobButton) throw new Error("missing graph job button");
    await userEvent.click(jobButton);

    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent",
      );
    });
    expect(await screen.findByText("runner job inspector")).toBeInTheDocument();
    expect(screen.getByText("planned")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Checkout workspace/ })).toBeInTheDocument();
    expect(screen.getByText("Click a step to see its logs.")).toBeInTheDocument();

    await userEvent.click(within(screen.getByLabelText("runner job steps")).getByRole("button", { name: /Run agent/ }));
    await waitFor(() => {
      expect(screen.getByTestId("path")).toHaveTextContent(
        "/projects/ambience/issues/172/runs/7/cycles/1/phases/agent-execute/jobs/agent/steps/run-agent",
      );
    });
  });

  it("does not render workflow as an issue tab", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url =
        typeof input === "string"
          ? new URL(input, "https://glimmung.test")
          : input instanceof URL
            ? input
            : new URL(input.url);
      if (url.pathname === "/v1/issues/by-number/ambience/172") return json(issueDetail);
      if (url.pathname === "/v1/issues/by-number/ambience/172/graph") return json(issueGraph);
      if (url.pathname === "/v1/workflows") return json([agentWorkflow]);
      throw new Error(`unhandled fetch ${url.pathname}`);
    }));

    renderIssueDetail("/projects/ambience/issues/172/settings", runtimeContext);

    expect(await screen.findByLabelText("issue sections")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "workflow" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "settings" })).toHaveAttribute("aria-pressed", "true");
  });
});

function renderIssueDetail(
  initialPath: string,
  outletContext: unknown = { signedIn: true, isAdmin: true, snap: { projects: [], workflows: [] } },
) {
  function TestLayout() {
    const location = useLocation();
    return (
      <>
        <div data-testid="path">{location.pathname}</div>
        <Outlet context={outletContext} />
      </>
    );
  }

  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route element={<TestLayout />}>
          <Route path="/projects/:project/issues/:issueNumber" element={<IssueDetailView />}>
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.summary} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runs} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.run} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runCycle} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runPhase} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runJob} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.runStep} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.settings} element={null} />
            <Route path={ISSUE_DETAIL_CHILD_ROUTES.review} element={null} />
          </Route>
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

function activeAgentProjection() {
  return {
    ...runProjection,
    runs: [{
      ...runProjection.runs[0],
      current_phase: "agent-execute",
      phases: runProjection.runs[0].phases.map((phase) => {
        if (phase.name === "env-prep") {
          return {
            ...phase,
            state: "succeeded",
            jobs: phase.jobs.map((job) => ({
              ...job,
              state: "succeeded",
              steps: job.steps.map((step) => ({ ...step, state: "succeeded" })),
            })),
          };
        }
        if (phase.name === "agent-execute") {
          return {
            ...phase,
            state: "active",
            jobs: phase.jobs.map((job) => ({
              ...job,
              name: "llm agent",
              state: "active",
              steps: job.steps.map((step) => ({
                ...step,
                state: step.slug === "run-agent" ? "active" : "succeeded",
              })),
            })),
            attempts: [{
              attempt_index: 0,
              state: "active",
              conclusion: null,
              verification_status: null,
              decision: null,
              log_archive_url: null,
              evidence_refs: [],
              job_completions: [],
            }],
          };
        }
        return phase;
      }),
    }],
  };
}

function abortedProjection() {
  return {
    ...runProjection,
    runs: [{
      ...runProjection.runs[0],
      state: "aborted",
      current_phase: "env-prep",
      abort_reason: "baselib_missing_or_unversioned",
      phases: runProjection.runs[0].phases.map((phase) => {
        if (phase.name !== "env-prep") return phase;
        return {
          ...phase,
          state: "failed",
          reason: "job_failed",
          jobs: phase.jobs.map((job) => ({
            ...job,
            state: "failed",
            reason: "aborted",
            steps: [
              { slug: "mint-credentials", title: "Mint credentials", state: "succeeded", exit_code: 0 },
              {
                slug: "probe-mod-set",
                title: "Verify allowed mods",
                state: "aborted",
                reason: "baselib_missing_or_unversioned",
              },
              { slug: "emit-env-outputs", title: "Emit env outputs", state: "not_started" },
            ],
          })),
        };
      }),
    }],
  };
}

// A terminally-failed verify run: the verify phase owns the failure, with a
// red `verdict` owner step, a typed terminal_observation, an abort_reason, and
// the deciding attempt's structured verification failure. This is the
// spirelens#147-shaped run from the motivating bug — clicking the failed verify
// job must surface the SPECIFIC cause in the inspector, not just green steps.
function verifierFailedProjection() {
  const base = runProjection.runs[0];
  const verifyPhase = {
    name: "llm-verify",
    kind: "k8s_job",
    state: "failed",
    reason: "verification_failed",
    verify: true,
    run_on: "success",
    purpose: "verify",
    depends_on: ["agent-execute"],
    jobs: [{
      id: "verify",
      name: "Verify effect",
      state: "failed",
      reason: "verification_failed",
      k8s_job_name: "glim-ambience-172-runs-7-1-0-llm-verify",
      started_at: "2026-05-20T17:40:00.000Z",
      completed_at: "2026-05-20T17:42:00.000Z",
      steps: [
        {
          slug: "run-verify",
          title: "Run verification",
          state: "succeeded",
          exit_code: 0,
          started_at: "2026-05-20T17:40:00.000Z",
          completed_at: "2026-05-20T17:42:00.000Z",
        },
        // The synthesized verdict owner step now carries the deciding verifier
        // reason as its message (server.RunProjectionStep.Message) — the step
        // pane renders this instead of "No hot runner events recorded".
        { slug: "verdict", title: "Verdict", state: "failed", reason: "verification_failed", message: "claimed_result_not_observed" },
      ],
    }],
    attempts: [{
      attempt_index: 0,
      state: "completed",
      conclusion: "failure",
      verification_status: "fail",
      decision: "abort",
      log_archive_url: null,
      evidence_refs: [],
      // The deciding attempt's verifier prose, now carried into the projection
      // so the verdict view surfaces the rich "why".
      summary_markdown: "The Cloak Clasp effect label reads \"Cloak Clasp\" but the verifier expected id CLOAK_CLASP; the stat was not observed.",
      job_completions: [{
        job_id: "verify",
        completed_at: "2026-05-20T17:42:00.000Z",
        conclusion: "failure",
        verification_status: "fail",
        verification_reasons: ["claimed_result_not_observed"],
        verification_failure: {
          expected: "CLOAK_CLASP",
          observed: "Cloak Clasp",
          where: "effect label",
          suspected_cause: "claimed_result_not_observed",
          cause_detail: "case/format mismatch between claimed and observed effect id",
        },
      }],
    }],
  };
  return {
    ...runProjection,
    runs: [{
      ...base,
      state: "aborted",
      current_phase: "llm-verify",
      abort_reason: "verification_failed",
      terminal_observation: {
        class: "verifier_failed",
        phase: "llm-verify",
        job_id: "verify",
        step_slug: "verdict",
        conclusion: "failure",
        reason: "claimed_result_not_observed",
        source: "completion_callback",
        message: 'claimed result not observed: expected "CLOAK_CLASP", observed "Cloak Clasp"',
      },
      topology: {
        ...base.topology,
        phases: [...base.topology.phases, {
          name: "llm-verify",
          kind: "k8s_job",
          verify: true,
          run_on: "success",
          purpose: "verify",
          depends_on: ["agent-execute"],
          jobs: [{ id: "verify", name: "Verify effect" }],
        }],
      },
      phases: [...base.phases, verifyPhase],
    }],
  };
}

// ALL_TERMINAL_OBSERVATION_CLASSES mirrors the Go canonical source of truth,
// `AllTerminalObservationClasses` in internal/server/terminal_observation.go.
// The inspector's `.run-failure-cause` render is class-agnostic, so this list is
// the frontend half of the cross-language regression guard: every terminal
// failure class must render a non-blank cause, and a class added to the Go list
// without being added here (and to the hardcoded expected set in
// "the terminal class list matches the canonical set") fails the frontend
// inventory test below. There is no runtime bridge between Go and TS, so the
// list is asserted against an independent hardcoded literal — the same tripwire
// shape as the Go side — to catch drift.
const ALL_TERMINAL_OBSERVATION_CLASSES = [
  "producer_phase_failed",
  "verifier_contract_missing",
  "verifier_failed",
  "gate_failed",
  "dispatch_failed",
  "phase_requested_abort",
  "manual_abort",
  "malformed_terminal",
] as const;

// verifierFailedProjectionForClass reuses slice 3's terminally-failed verify
// fixture but stamps a chosen terminal_observation class onto it. The render is
// class-agnostic, so this proves the `.run-failure-cause` block surfaces the
// class for EVERY terminal class — no class renders blank.
function verifierFailedProjectionForClass(terminalClass: string) {
  const base = verifierFailedProjection();
  const run = base.runs[0];
  return {
    ...base,
    runs: [{
      ...run,
      terminal_observation: {
        ...run.terminal_observation,
        class: terminalClass,
        message: `terminal failure attributed to ${terminalClass}`,
      },
    }],
  };
}

function agentPageEvent(seq: number, content: unknown[]) {
  return {
    project: "ambience",
    run_ref: "ambience#172/runs/7.1",
    attempt_index: 0,
    phase: "agent-execute",
    job_id: "agent",
    seq,
    event: "log",
    step_slug: "run-agent",
    message: JSON.stringify({
      type: "assistant",
      message: { content },
    }),
    exit_code: null,
    metadata: { stream: "stdout" },
    created_at: "2026-05-20T17:24:10.000Z",
  };
}

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("runnerTerminalText synthesized owner step", () => {
  it("never renders the no-events dead-end for a verdict owner step", () => {
    // The pure-function belt under the structured VerdictStepDetail render: a
    // synthesized owner step has no hot events by construction, so the dead-end
    // string is a lie. It must carry the failure detail instead.
    const verdictStep = {
      slug: "verdict",
      title: "Verdict",
      state: "failed",
      reason: "verification_failed",
      message: "verifier reported status=abort reason=claimed_result_not_observed",
    };
    const job = { job_id: "verify", name: "Verify effect", state: "failed", steps: [verdictStep] };

    const out = runnerTerminalText(job, verdictStep, []);
    expect(out).not.toContain("No hot runner events recorded");
    expect(out).toContain("verifier reported status=abort reason=claimed_result_not_observed");
    expect(out).toContain("# reason verification_failed");
  });

  it("still reports no events for a real runner step with an empty stream", () => {
    // Guard the inverse: a real step that genuinely recorded nothing keeps the
    // honest "no events" message — the belt is owner-step-scoped, not blanket.
    const realStep = { slug: "run-verification", title: "Run verification", state: "succeeded" };
    const job = { job_id: "verify", name: "Verify effect", state: "failed", steps: [realStep] };

    const out = runnerTerminalText(job, realStep, []);
    expect(out).toContain("No hot runner events recorded for this selection.");
  });
});

describe("agentTranscriptEntries codex projection", () => {
  const mkEvent = (seq: number, message: string): RunnerEvent => ({
    project: "ambience",
    run_ref: "ambience#164/runs/7.1",
    attempt_index: 0,
    phase: "llm-work",
    job_id: "llm-implement",
    seq,
    event: "log",
    step_slug: "run-implementation",
    message,
    exit_code: null,
    metadata: {},
    created_at: "2026-06-14T08:40:00Z",
  });

  it("projects codex item.completed events (agent_message, command, file_change) into transcript entries", () => {
    const entries = agentTranscriptEntries([
      mkEvent(1, JSON.stringify({ type: "thread.started" })),
      mkEvent(2, JSON.stringify({ type: "item.completed", item: { id: "item_0", type: "agent_message", text: "I will inspect the repo." } })),
      // item.started is a streaming duplicate of item.completed and must not double-count.
      mkEvent(3, JSON.stringify({ type: "item.started", item: { id: "item_1", type: "command_execution", command: "pwd", status: "in_progress" } })),
      mkEvent(4, JSON.stringify({ type: "item.completed", item: { id: "item_1", type: "command_execution", command: "ls", aggregated_output: "rain.go\n", exit_code: 0, status: "completed" } })),
      mkEvent(5, JSON.stringify({ type: "item.completed", item: { id: "item_64", type: "file_change", changes: [{ path: "/workspace/repo/sim/rain_on_window.go", kind: "add" }], status: "completed" } })),
    ]);

    // The regression this fixes: codex agent_message text now surfaces as an
    // assistant entry. Previously every codex event was dropped as raw, leaving
    // codex transcripts empty (only stray stderr lines survived).
    const assistant = entries.filter((entry) => entry.kind === "assistant");
    expect(assistant).toHaveLength(1);
    expect(assistant[0].text).toBe("I will inspect the repo.");

    // command_execution -> tool_call + tool_result; file_change -> tool_call.
    const toolCalls = entries.filter((entry) => entry.kind === "tool_call");
    expect(toolCalls.map((entry) => entry.toolName)).toEqual(["command", "file_change"]);
    expect(entries.find((entry) => entry.kind === "tool_result")?.text).toBe("rain.go\n");

    // item.started must not double-count, and thread.started yields no entry.
    expect(entries).toHaveLength(4);
  });
});
