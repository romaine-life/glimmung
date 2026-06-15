import { describe, expect, it } from "vitest";

import {
  runTopologyToPhaseGraphModel,
  workflowToPhaseGraphModel,
  type WorkflowGraphSource,
} from "./workflowGraphModel";

describe("workflowToPhaseGraphModel", () => {
  it("maps registered phases without leaking recycle policy fields into graph phases", () => {
    const workflow: WorkflowGraphSource = {
      name: "default",
      phases: [
        {
          name: "implementation",
          kind: "k8s_job",
          verify: true,
          depends_on: [],
          jobs: [
            { id: "plan", name: "plan" },
            { id: "implement", name: "implement", image: "runner:latest" },
          ],
          recycle_policy: {
            max_attempts: 2,
            on: ["failed", "needs_review"],
            lands_at: "self",
          },
        },
        {
          name: "review",
          kind: "k8s_job",
          run_on: "success",
          purpose: "review",
          depends_on: ["implementation"],
          jobs: [{ id: "pr-review", name: "PR review", primitive: "pr_review" }],
        },
      ],
      pr: { recycle_policy: null },
    };

    expect(workflowToPhaseGraphModel(workflow, { recycleActive: true })).toEqual({
      phases: [
        {
          name: "implementation",
          kind: "k8s_job",
          verify: true,
          run_on: undefined,
          purpose: undefined,
          evidence_verification_gate: undefined,
          depends_on: [],
          jobs: [
            { id: "plan", name: "plan", image: undefined, primitive: undefined },
            { id: "implement", name: "implement", image: "runner:latest", primitive: undefined },
          ],
        },
        {
          name: "review",
          kind: "k8s_job",
          verify: undefined,
          run_on: "success",
          purpose: "review",
          evidence_verification_gate: undefined,
          depends_on: ["implementation"],
          jobs: [{ id: "pr-review", name: "PR review", image: undefined, primitive: "pr_review" }],
        },
      ],
      entryArrows: [{
        target: "implementation",
        active: false,
        kind: "default",
      }],
      recycleArrows: [
        {
          source: "implementation",
          target: "implementation",
          trigger: "failed / needs_review",
          max_attempts: 2,
          active: true,
          kind: "phase_recycle",
        },
      ],
    });
  });

  it("sources pr recycle arrows from the registered pr_review phase", () => {
    const workflow: WorkflowGraphSource = {
      name: "ambience",
      phases: [
        { name: "prepare", kind: "k8s_job" },
        { name: "llm-work", kind: "k8s_job", depends_on: ["prepare"] },
        {
          name: "evidence-gate",
          kind: "k8s_job",
          depends_on: ["llm-work"],
          recycle_policy: {
            max_attempts: 3,
            on: ["verify_fail"],
            lands_at: "prepare",
          },
        },
        {
          name: "review-surface",
          kind: "k8s_job",
          run_on: "success",
          purpose: "review",
          depends_on: ["evidence-gate"],
          jobs: [{ id: "pr-review", primitive: "pr_review" }],
        },
      ],
      pr: {
        recycle_policy: {
          max_attempts: 3,
          on: ["changes_requested"],
          lands_at: "prepare",
        },
      },
    };

    const model = workflowToPhaseGraphModel(workflow);
    expect(model.entryArrows).toEqual([{
      target: "prepare",
      active: false,
      kind: "default",
    }]);
    expect(model.recycleArrows).toEqual([
      {
        source: "evidence-gate",
        target: "prepare",
        trigger: "verify_fail",
        max_attempts: 3,
        active: false,
        kind: "phase_recycle",
      },
      {
        source: "review-surface",
        target: "prepare",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "review_recycle",
      },
    ]);
  });

  it("keeps workflow step group metadata in graph jobs", () => {
    const workflow: WorkflowGraphSource = {
      name: "default",
      phases: [{
        name: "verify",
        kind: "k8s_job",
        jobs: [{
          id: "verify-ui",
          steps: [{
            slug: "capture-screenshot",
            title: "capture screenshot",
            group: "sweep-01",
            group_title: "sweep 01",
          }],
        }],
      }],
      pr: { recycle_policy: null },
    };

    expect(workflowToPhaseGraphModel(workflow).phases[0].jobs?.[0].steps?.[0]).toMatchObject({
      slug: "capture-screenshot",
      group: "sweep-01",
      group_title: "sweep 01",
    });
  });
});

describe("runTopologyToPhaseGraphModel", () => {
  it("uses run projection topology as the execution graph shape", () => {
    expect(runTopologyToPhaseGraphModel({
      phases: [
        {
          name: "prepare",
          kind: "k8s_job",
          verify: false,
          run_on: "success",
          purpose: "work",
          depends_on: [],
          jobs: [{ id: "prepare", name: "Prepare env" }],
        },
        {
          name: "review",
          kind: "k8s_job",
          verify: false,
          run_on: "success",
          purpose: "review",
          depends_on: ["prepare"],
          jobs: [{ id: "pr-review", name: "PR review" }],
        },
      ],
      default_entry: { target: "prepare", active: true, kind: "default" },
      recycle_arrows: [{
        source: "review",
        target: "prepare",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "review_recycle",
      }],
    })).toEqual({
      phases: [
        {
          name: "prepare",
          kind: "k8s_job",
          verify: false,
          run_on: "success",
          purpose: "work",
          depends_on: [],
          jobs: [{ id: "prepare", name: "Prepare env", image: undefined }],
        },
        {
          name: "review",
          kind: "k8s_job",
          verify: false,
          run_on: "success",
          purpose: "review",
          depends_on: ["prepare"],
          jobs: [{ id: "pr-review", name: "PR review", image: undefined }],
        },
      ],
      entryArrows: [{
        target: "prepare",
        active: true,
        kind: "default",
      }],
      recycleArrows: [{
        source: "review",
        target: "prepare",
        trigger: "changes_requested",
        max_attempts: 3,
        active: false,
        kind: "review_recycle",
      }],
    });
  });

  it("keeps run topology step group metadata in graph jobs", () => {
    const model = runTopologyToPhaseGraphModel({
      phases: [{
        name: "verify",
        kind: "k8s_job",
        jobs: [{
          id: "verify-ui",
          steps: [{
            slug: "judge-evidence",
            title: "judge evidence",
            group: "sweep-01",
            group_title: "sweep 01",
          }],
        }],
      }],
      default_entry: { target: "verify", active: true, kind: "default" },
      recycle_arrows: [],
    });

    expect(model.phases[0].jobs?.[0].steps?.[0]).toMatchObject({
      slug: "judge-evidence",
      group: "sweep-01",
      group_title: "sweep 01",
    });
  });
});
