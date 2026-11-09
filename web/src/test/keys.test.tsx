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
    await user.type(within(dialog).getByLabelText("Requests per minute"), "300");
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
});
