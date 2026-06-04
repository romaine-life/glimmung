import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import { Shell, type ShellSnapshot } from "./Shell";

const snap: ShellSnapshot = {
  active_leases: [],
  test_environments: [],
  waiting_test_slot_requests: [],
  projects: [{ name: "glimmung", github_repo: "romaine-life/glimmung" }],
};

function renderShell() {
  return render(
    <MemoryRouter>
      <Shell
        snap={snap}
        account={{
          signedIn: true,
          name: "Curator",
          email: "curator@example.com",
          avatarUrl: "https://www.gravatar.com/avatar/b58996c504c5638798eb6b511e6f49af?s=64&d=mp",
          isAdmin: true,
        }}
      >
        <main>content</main>
      </Shell>
    </MemoryRouter>,
  );
}

describe("Shell", () => {
  it("collapses the left sidebar from the top control", async () => {
    const user = userEvent.setup();
    const { container } = renderShell();

    expect(container.querySelector(".app")).not.toHaveClass("sidebar-collapsed");

    await user.click(screen.getByRole("button", { name: "collapse sidebar" }));

    expect(container.querySelector(".app")).toHaveClass("sidebar-collapsed");
    expect(screen.getByRole("button", { name: "open sidebar" })).toHaveAttribute("aria-pressed", "true");
  });

  it("renders the account Gravatar image and falls back to initials on error", () => {
    const { container } = renderShell();

    const avatar = container.querySelector(".avatar-image img");
    expect(avatar).toHaveAttribute(
      "src",
      "https://www.gravatar.com/avatar/b58996c504c5638798eb6b511e6f49af?s=64&d=mp",
    );

    fireEvent.error(avatar!);

    expect(screen.getByText("CU")).toBeInTheDocument();
  });
});
