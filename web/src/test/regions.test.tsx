import { act, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { finishBuilds } from "../mocks/server";
import { renderApp } from "./util";

describe("add region flow", () => {
  it("first run: empty → search catalog → estimate → add and build → progress → live", async () => {
    const { user } = renderApp("/regions", { authed: true, empty: true });
    expect(await screen.findByText("No regions yet")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Add a region" }));

    const dialog = await screen.findByRole("dialog", { name: "Add a region" });
    await user.type(await within(dialog).findByLabelText("Search regions"), "dela");
    expect(within(dialog).queryByText("Texas")).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: /^Delaware/ }));

    const est = within(dialog).getByTestId("estimate");
    expect(est).toHaveTextContent("Delaware");
    expect(est).toHaveTextContent(/Total\s*≈/);
    expect(est).toHaveTextContent("USGS 3DEP 1/3″ · 10 m");

    await user.click(within(dialog).getByRole("button", { name: "Add and build" }));
    // lands on Builds with a live job
    expect(await screen.findByRole("heading", { name: "Builds" })).toBeInTheDocument();
    expect(await screen.findByText("Current build")).toBeInTheDocument();
    expect(screen.getByRole("log", { name: "Build log" })).toBeInTheDocument();

    act(() => finishBuilds());
    expect(await screen.findByText("Live")).toBeInTheDocument();
    expect(await screen.findByText(/Idle/)).toBeInTheDocument();
  });

  it("adds a custom box typed as coordinates", async () => {
    const { user } = renderApp("/regions?add=1", { authed: true });
    const dialog = await screen.findByRole("dialog", { name: "Add a region" });
    await user.click(within(dialog).getByRole("radio", { name: /Draw a box/ }));
    await user.type(await within(dialog).findByLabelText("Name"), "Permian north");
    await user.type(within(dialog).getByLabelText(/Bounding box/), "-103.8, 31.9, -102.1, 33.1");
    const est = within(dialog).getByTestId("estimate");
    expect(est).toHaveTextContent("Permian north");
    // The box crosses the Texas/New Mexico line: clipped from both, not from the whole US.
    const parents = within(dialog).getByTestId("parents");
    await waitFor(() => expect(parents).toHaveTextContent(/Clipped from New Mexico .* \+ Texas/));
    expect(parents).toHaveTextContent(/downloads .* instead of United States/);
    await user.click(within(dialog).getByRole("button", { name: "Add only" }));
    expect(await screen.findByText(/added but not built yet/)).toBeInTheDocument();
    // in the banner and in the table
    expect(screen.getAllByText("Permian north")).toHaveLength(2);
    expect(screen.getByText("Not built")).toBeInTheDocument();
  });
});
