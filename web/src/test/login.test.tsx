import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderApp } from "./util";

describe("login", () => {
  it("rejects a wrong password, then signs in", async () => {
    const { user } = renderApp("/keys", { authed: false });
    const username = await screen.findByLabelText("Username");
    await user.type(username, "admin");
    await user.type(screen.getByLabelText("Password"), "nope");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Wrong username or password");

    await user.clear(screen.getByLabelText("Password"));
    await user.type(screen.getByLabelText("Password"), "admin");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "API keys" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
  });

  it("goes back to the login form when signing out", async () => {
    const { user } = renderApp("/settings");
    await screen.findByRole("heading", { name: "Settings" });
    await user.click(screen.getAllByRole("button", { name: "Sign out" })[0]);
    expect(await screen.findByLabelText("Username")).toBeInTheDocument();
  });
});
