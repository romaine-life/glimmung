import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { VerificationVerdict, hasVerdict } from "./VerificationVerdict";
import { projectionJobVerdict } from "./IssueDetailView";

afterEach(cleanup);

type Attempt = Parameters<typeof projectionJobVerdict>[0];
type Job = Parameters<typeof projectionJobVerdict>[1];

const attempt = (over: Record<string, unknown>): Attempt =>
  ({ attempt_index: 0, state: "failed", evidence_refs: [], ...over }) as unknown as Attempt;
const job = (id: string): Job => ({ id, state: "failed", steps: [] }) as unknown as Job;

describe("hasVerdict", () => {
  it("is false for null or all-empty verdicts", () => {
    expect(hasVerdict(null)).toBe(false);
    expect(hasVerdict({ status: "", failure: null, reasons: [] })).toBe(false);
    // whitespace-only fields do not count as a verdict
    expect(hasVerdict({ status: "  ", failure: { expected: "  " }, reasons: ["  "] })).toBe(false);
  });

  it("is true when a status, any failure field, or a reason is present", () => {
    expect(hasVerdict({ status: "fail" })).toBe(true);
    expect(hasVerdict({ failure: { observed: "blank screen" } })).toBe(true);
    expect(hasVerdict({ reasons: ["404 on route"] })).toBe(true);
  });
});

describe("projectionJobVerdict", () => {
  it("returns the selected job's verdict with its structured failure block", () => {
    const a = attempt({
      verification_status: "fail",
      job_completions: [
        { job_id: "env-prep", conclusion: "success" },
        {
          job_id: "llm-verify",
          conclusion: "failure",
          verification_status: "fail",
          verification_failure: {
            expected: "stars connect into a figure",
            observed: "sparse starfield",
            where: "/dev/constellations",
            suspected_cause: "code_bug",
          },
          verification_reasons: [],
        },
      ],
    });
    const verdict = projectionJobVerdict(a, job("llm-verify"));
    expect(verdict?.status).toBe("fail");
    expect(verdict?.failure?.observed).toContain("sparse starfield");
  });

  it("returns null for a non-verify job so the panel stays absent", () => {
    const a = attempt({ job_completions: [{ job_id: "env-prep", conclusion: "success" }] });
    expect(projectionJobVerdict(a, job("env-prep"))).toBeNull();
  });

  it("falls back to the attempt-level status when the job has no completion row", () => {
    const a = attempt({ verification_status: "fail", job_completions: [] });
    expect(projectionJobVerdict(a, job("llm-verify"))?.status).toBe("fail");
  });

  it("returns null when there is no attempt", () => {
    expect(projectionJobVerdict(null, job("llm-verify"))).toBeNull();
  });
});

describe("<VerificationVerdict/>", () => {
  it("renders the expected/observed/suspected-cause diagnosis for a failure", () => {
    render(
      <VerificationVerdict
        verdict={{
          status: "fail",
          failure: {
            expected: "a drawn constellation figure",
            observed: "only a sparse starfield",
            where: "/dev/constellations",
            suspected_cause: "code_bug",
            cause_detail: "no line draw scheduled",
          },
          reasons: [],
        }}
      />,
    );
    expect(screen.getByText("fail")).toBeInTheDocument();
    expect(screen.getByText(/a drawn constellation figure/)).toBeInTheDocument();
    expect(screen.getByText(/only a sparse starfield \[\/dev\/constellations\]/)).toBeInTheDocument();
    expect(screen.getByText("code_bug")).toBeInTheDocument();
    expect(screen.getByText(/no line draw scheduled/)).toBeInTheDocument();
  });

  it("renders reason lines when there is no structured failure block", () => {
    render(<VerificationVerdict verdict={{ status: "fail", failure: null, reasons: ["route 404", "no canvas"] }} />);
    expect(screen.getByText("route 404")).toBeInTheDocument();
    expect(screen.getByText("no canvas")).toBeInTheDocument();
  });

  it("renders nothing when the verdict is empty", () => {
    const { container } = render(<VerificationVerdict verdict={{ status: "", failure: null, reasons: [] }} />);
    expect(container.firstChild).toBeNull();
  });
});
