import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderApp } from "./util";

describe("API keys", () => {
  it("shows a new key's secret exactly once", async () => {
    const { user } = renderApp("/keys");
    await screen.findByText("Field app (mobile)");

    await user.click(screen.getByRole("button", { name: "Create key" }));
    const dialog = await screen.findByRole("dialog", { name: "Create API key" });
    await user.type(within(dialog).getByLabelText("Label"), "Dispatch board");
    await user.type(within(dialog).getByLabelText("Rate (units per second)"), "5");
    await user.type(within(dialog).getByLabelText("Daily quota (units)"), "1000");
    // the quota is explained in requests of each kind
    expect(within(dialog).getByText(/1,000 vector tiles, or 100 raster tiles/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Create key" }));

    const secretEl = await screen.findByTestId("secret");
    const secret = secretEl.textContent ?? "";
    expect(secret).toMatch(/^mt_[A-Za-z0-9]{20,}$/);
    // usage snippets carry the secret and follow the chosen style
    expect(
      screen.getByText(`${location.origin}/styles/overlay-dark.json?key=${secret}`),
    ).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("Style for the snippets"), "satellite");
    expect(
      screen.getByText(`${location.origin}/styles/satellite.json?key=${secret}`),
    ).toBeInTheDocument();
    // cannot be dismissed by Escape: only the explicit button
    await user.keyboard("{Escape}");
    expect(screen.getByTestId("secret")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "I have saved it" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    // listed by label and prefix, never the full secret
    expect(await screen.findByText("Dispatch board")).toBeInTheDocument();
    // its limits, with the burst the server chose
    expect(screen.getByText(/5\/s, burst 10 · 1,000\/day/)).toBeInTheDocument();
    expect(screen.getByText(`${secret.slice(0, 7)}…`)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(secret);

    // reopening the dialog starts a fresh form, no secret
    await user.click(screen.getByRole("button", { name: "Create key" }));
    expect(await screen.findByRole("dialog", { name: "Create API key" })).toBeInTheDocument();
    expect(screen.queryByTestId("secret")).not.toBeInTheDocument();
  });

  it("revokes a key after confirmation", async () => {
    const { user } = renderApp("/keys");
    await screen.findByText("Old test key");
    await user.click(screen.getByRole("button", { name: "Revoke Old test key" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Revoke key" }));
    await waitFor(() => expect(screen.queryByText("Old test key")).not.toBeInTheDocument());
  });

  it("shows usage against each limit and what was refused", async () => {
    const { user } = renderApp("/keys");
    const row = (await screen.findByText("Field app (mobile)")).closest("tr") as HTMLElement;
    expect(within(row).getByText("37 refused")).toBeInTheDocument();
    await user.click(
      screen.getByRole("button", { name: "Usage and limits of Field app (mobile)" }),
    );
    const dialog = await screen.findByRole("dialog", { name: "Field app (mobile)" });
    const usage = within(dialog).getByRole("region", { name: "Usage against limits" });
    expect(within(usage).getByText("Today (units, UTC day)")).toBeInTheDocument();
    expect(within(usage).getByText("212,400")).toBeInTheDocument();
    expect(within(usage).getByText("of 250,000")).toBeInTheDocument();
    expect(within(usage).getByText("This month (units)")).toBeInTheDocument();
    const bars = within(usage).getAllByRole("progressbar");
    expect(bars.map((b) => b.getAttribute("aria-valuenow"))).toEqual(["85", "46", "18"]);
    const refused = within(dialog).getByRole("region", { name: "Refused requests" });
    expect(refused).toHaveTextContent("37 today");
    expect(refused).toHaveTextContent("31 one client over the per-client rate; 6 over its rate");
    expect(within(refused).getAllByText("203.0.113.9").length).toBe(3);
  });

  it("edits a key's limits, and removes one by emptying its field", async () => {
    const { user } = renderApp("/keys");
    await screen.findByText("Field app (mobile)");
    await user.click(
      screen.getByRole("button", { name: "Usage and limits of Field app (mobile)" }),
    );
    const dialog = await screen.findByRole("dialog", { name: "Field app (mobile)" });
    const rate = within(dialog).getByLabelText("Rate (units per second)");
    expect(rate).toHaveValue(20);
    // a burst with no rate is refused before anything is sent
    await user.clear(rate);
    await user.click(within(dialog).getByRole("button", { name: "Save limits" }));
    expect(await within(dialog).findByText("A burst needs a rate.")).toBeInTheDocument();
    await user.type(rate, "8");
    await user.clear(within(dialog).getByLabelText("Burst (units)"));
    await user.clear(within(dialog).getByLabelText("Monthly quota (units)"));
    await user.click(within(dialog).getByRole("button", { name: "Save limits" }));
    expect(await within(dialog).findByRole("status")).toHaveTextContent("Saved");
    await user.click(within(dialog).getAllByRole("button", { name: "Close" })[1]);
    const row = screen.getByText("Field app (mobile)").closest("tr") as HTMLElement;
    await waitFor(() =>
      expect(row).toHaveTextContent("8/s, burst 16 · 250,000/day · 5/s per client"),
    );
    expect(row).not.toHaveTextContent("/month");
  });

  it("edits what requests cost and the server limits", async () => {
    const { user } = renderApp("/keys");
    const cost = await screen.findByLabelText("Cost of Raster tiles");
    expect(cost).toHaveValue(10);
    expect(screen.getByLabelText("Server limit for Vector tiles, styles, fonts")).toHaveValue(null);
    await user.clear(cost);
    await user.type(cost, "0");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(
      await screen.findByText(/Raster tiles: the cost must be a whole number/),
    ).toBeInTheDocument();
    await user.clear(cost);
    await user.type(cost, "25");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/Saved\. It applies/)).toBeInTheDocument();
    // the create form now explains costs with the new weight
    await user.click(screen.getByRole("button", { name: "Create key" }));
    const dialog = await screen.findByRole("dialog", { name: "Create API key" });
    await waitFor(() => expect(dialog).toHaveTextContent("a raster tile 25"));
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Reset to defaults" }));
    expect(screen.getByLabelText("Cost of Raster tiles")).toHaveValue(10);
  });
});
