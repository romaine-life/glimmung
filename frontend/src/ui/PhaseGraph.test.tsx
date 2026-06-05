import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { PhaseGraph } from "./PhaseGraph";

describe("workflow PhaseGraph", () => {
  it("renders readable phase names while preserving raw identifiers", () => {
    render(
      <PhaseGraph
        phases={[
          { name: "evidence-gate", kind: "k8s_job" },
          { name: "touchpoint_gate", kind: "k8s_job" },
        ]}
        recycles={[]}
      />,
    );

    expect(screen.getByText("Evidence gate")).toHaveAttribute("title", "evidence-gate");
    expect(screen.getByText("Touchpoint gate")).toHaveAttribute("title", "touchpoint_gate");
    expect(screen.getAllByText("k8s_job")).toHaveLength(2);
  });
});
