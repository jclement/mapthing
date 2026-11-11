import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Setting } from "../api";
import { countSet, groupSettings, shownValue } from "../lib/settings";
import { renderApp } from "./util";

const s = (over: Partial<Setting>): Setting => ({
  name: "MAPTHING_X",
  group: "Serving",
  value: "v",
  default: "d",
  source: "default",
  set: false,
  help: "h",
  ...over,
});

describe("settings logic", () => {
  it("groups in the server's order, with mistakes first", () => {
    const groups = groupSettings([
      s({ name: "A", group: "Access", source: "env", set: true }),
      s({ name: "B", group: "Serving" }),
      s({ name: "C", group: "Access" }),
      s({ name: "TYPO", group: "Not recognised", source: "env", set: true }),
    ]);
    expect(groups.map((g) => g.name)).toEqual(["Not recognised", "Access", "Serving"]);
    expect(groups[1].settings.map((x) => x.name)).toEqual(["A", "C"]);
    expect(groups[1].changed).toBe(1);
  });

  it("can show only what is set in the environment", () => {
    const all = [s({ name: "A", source: "env", set: true }), s({ name: "B", group: "Builds" })];
    const groups = groupSettings(all, true);
    expect(groups).toHaveLength(1);
    expect(groups[0].settings.map((x) => x.name)).toEqual(["A"]);
    expect(countSet(all)).toBe(1);
  });

  it("never prints a secret, set or not", () => {
    expect(shownValue(s({ secret: true, set: true, source: "env", value: "••••••••" }))).toEqual({
      text: "••••••••",
      muted: false,
    });
    expect(
      shownValue(s({ secret: true, set: false, value: "", default: "generated at each start" })),
    ).toEqual({ text: "not set (generated at each start)", muted: true });
    expect(shownValue(s({ source: "env", set: true, value: "https://x" }))).toEqual({
      text: "https://x",
      muted: false,
    });
    expect(shownValue(s({ value: ":8080" }))).toEqual({ text: ":8080", muted: true });
  });
});

describe("Settings page", () => {
  it("shows each value with where it came from, secrets hidden, versions listed", async () => {
    const { user } = renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(await screen.findByTestId("value-ADMIN_PASSWORD")).toHaveTextContent("••••••••");
    expect(screen.getByTestId("value-MAPTHING_PUBLIC_URL")).toHaveTextContent(
      "https://maps.example.com",
    );
    expect(screen.getByTestId("value-MAPTHING_LISTEN")).toHaveTextContent(":8080");
    expect(screen.getByTestId("value-MAPTHING_INTERNAL_TOKEN")).toHaveTextContent(
      "not set (generated at each start)",
    );
    // where each came from
    const url = screen.getByTestId("value-MAPTHING_PUBLIC_URL").closest("li") as HTMLElement;
    expect(within(url).getByText("set in the environment")).toBeInTheDocument();
    const listen = screen.getByTestId("value-MAPTHING_LISTEN").closest("li") as HTMLElement;
    expect(within(listen).getByText("set by the image")).toBeInTheDocument();
    const data = screen.getByTestId("value-MAPTHING_DATA").closest("li") as HTMLElement;
    expect(within(data).getByText("default")).toBeInTheDocument();
    // a misspelt variable is called out
    expect(screen.getByText(/mapthing does not read them/)).toBeInTheDocument();
    expect(screen.getByText("MAPTHING_TRUSTED_PROXI")).toBeInTheDocument();
    // versions
    const software = screen.getByRole("heading", { name: "Software" }).closest("section");
    for (const name of [
      "mapthing",
      "Valhalla",
      "Planetiler (Protomaps basemap)",
      "Renderer (MapLibre Native)",
    ])
      expect(within(software as HTMLElement).getByText(name)).toBeInTheDocument();
    expect(within(software as HTMLElement).getByText("3.9.1")).toBeInTheDocument();
    // nothing to edit
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();

    // only what is set
    await user.click(screen.getByRole("radio", { name: /Set here/ }));
    expect(screen.queryByTestId("value-MAPTHING_LISTEN")).not.toBeInTheDocument();
    expect(screen.getByTestId("value-MAPTHING_PUBLIC_URL")).toBeInTheDocument();
  });

  it("the old System address still works", async () => {
    renderApp("/system");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
  });
});
