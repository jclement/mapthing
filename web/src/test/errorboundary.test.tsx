import { screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderApp } from "./util";

// One page that always throws while rendering, as a real bug would.
vi.mock("../pages/Stats", () => ({
  StatsPage: () => {
    throw new Error("stats exploded");
  },
}));

describe("error boundary", () => {
  beforeEach(() => {
    // React and the boundary both log the caught error; keep the test output clean.
    vi.spyOn(console, "error").mockImplementation(() => {});
  });
  afterEach(() => vi.restoreAllMocks());

  it("shows the alert in place of a throwing page and keeps the sidebar", async () => {
    const { user } = renderApp("/stats");
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("This page hit an error");
    expect(alert).toHaveTextContent("stats exploded");

    // The shell survived: the desktop sidebar (first nav) is still there and still works.
    const sidebar = screen.getAllByRole("navigation")[0];
    expect(sidebar.closest("aside")).not.toBeNull();
    expect(within(sidebar).getByRole("link", { name: /Stats/ })).toBeInTheDocument();
    await user.click(within(sidebar).getByRole("link", { name: /System/ }));
    expect(await screen.findByRole("heading", { name: "System" })).toBeInTheDocument();
    expect(screen.queryByText("This page hit an error")).not.toBeInTheDocument();
  });
});
