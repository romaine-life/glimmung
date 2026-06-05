import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import { Shell, type ShellSnapshot } from "./Shell";

const snap: ShellSnapshot = {
  active_leases: [],
  test_environments: [],
  waiting_test_slot_requests: [],
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
    expect(screen.getByRole("button", { name: "open sidebar" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("collapse")).not.toBeInTheDocument();
    expect(screen.queryByText("open")).not.toBeInTheDocument();
  });

  it("offers Projects as a Work nav option, not a standalone switcher", () => {
    const { container } = renderShell();

    expect(container.querySelector(".project-switch")).not.toBeInTheDocument();

    const work = within(container).getByText("Work").closest(".nav-group");
    expect(work).not.toBeNull();
    const projectsNav = within(work as HTMLElement).getByRole("link", { name: "Projects" });
    expect(projectsNav).toHaveAttribute("href", "/projects");
    expect(projectsNav).toHaveClass("nav-item");
    expect(projectsNav.querySelector("use")).toHaveAttribute("href", "#ic-folder");
  });

  it("keeps sign-out affordances without footer status clutter", () => {
    const { container } = renderShell();

    expect(screen.queryByText("live")).not.toBeInTheDocument();
    expect(within(container).getByRole("button", { name: "sign out" }).querySelector("use")).toHaveAttribute("href", "#ic-logout");
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
