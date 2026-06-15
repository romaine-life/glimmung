import { describe, expect, it } from "vitest";

import { resolveProjectWorkflow } from "./workflowLookup";

const workflows = [
  { project: "glimmung", name: "default", id: 1 },
  { project: "glimmung", name: "review-agent", id: 2 },
  { project: "tank-operator", name: "default", id: 3 },
];

describe("resolveProjectWorkflow", () => {
  it("prefers an exact candidate match within the requested project", () => {
    expect(resolveProjectWorkflow(workflows, "glimmung", [null, "review-agent"])).toEqual({
      project: "glimmung",
      name: "review-agent",
      id: 2,
    });
  });

  it("falls back when the project has exactly one workflow", () => {
    expect(resolveProjectWorkflow(workflows, "tank-operator", ["missing"])).toEqual({
      project: "tank-operator",
      name: "default",
      id: 3,
    });
  });

  it("returns null for ambiguous projects without an exact match", () => {
    expect(resolveProjectWorkflow(workflows, "glimmung", ["missing"])).toBeNull();
  });
});

