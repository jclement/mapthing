import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { OnDemandStatus } from "../api";
import {
  appliesTo,
  cacheFill,
  cacheHitRate,
  cacheSizeProblem,
  gbToMb,
  hostState,
  mbToGb,
  onDemandTerrainBytes,
  sortedHosts,
  terrainMode,
} from "../lib/ondemand";
import { onDemandStatus } from "../mocks/fixtures";
import { renderApp } from "./util";

const status = (over: Partial<OnDemandStatus["config"]> = {}, regions: string[] = []) =>
  onDemandStatus({ outside: "off", cache_mb: 5000, ...over }, regions);

describe("on-demand logic", () => {
  it("reports how much is answered from the cache, and nothing before any request", () => {
    expect(cacheHitRate({ ...status().counters, hits: 0, built: 0 })).toBeNull();
    expect(cacheHitRate({ ...status().counters, hits: 90, built: 10 })).toBe(0.9);
  });

  it("measures the cache against its budget", () => {
    const s = status({}, ["a"]);
    expect(cacheFill(s)).toBeCloseTo(s.cache.bytes / s.cache.max_bytes);
    expect(cacheFill({ ...s, cache: { bytes: 10, max_bytes: 5, entries: 1 } })).toBe(1);
    expect(cacheFill({ ...s, cache: { bytes: 10, max_bytes: 0, entries: 1 } })).toBe(0);
  });

  it("says where it applies", () => {
    const names = { a: "Texas", b: "Alberta" };
    expect(appliesTo(status(), names)).toBe("nowhere");
    expect(appliesTo(status({}, ["a"]), names)).toBe("Texas");
    expect(appliesTo(status({}, ["a", "b"]), names)).toBe("Texas and Alberta");
    expect(appliesTo(status({ outside: "on_demand" }), names)).toBe(
      "everywhere outside built regions",
    );
    expect(appliesTo(status({ outside: "on_demand" }, ["a"]), names)).toBe(
      "Texas and everywhere outside built regions",
    );
    expect(appliesTo(status({}, ["a", "b", "c", "d", "e"]), names)).toBe(
      "Texas, Alberta and 3 more regions",
    );
  });

  it("puts a host that stopped answering first and says so", () => {
    const failing = onDemandStatus({ outside: "on_demand", cache_mb: 5000 }, [], false, true);
    const hosts = sortedHosts(failing.upstream);
    expect(hosts[0].healthy).toBe(false);
    expect(hostState(hosts[0])).toEqual({ tone: "bad", label: "Not answering" });
    expect(hostState(hosts[1])).toEqual({ tone: "ok", label: "Answering" });
    expect(hostState({ ...hosts[1], requests: 0 })).toEqual({
      tone: "neutral",
      label: "Not used yet",
    });
    expect(failing.healthy).toBe(false);
  });

  it("converts and checks the cache size", () => {
    expect(mbToGb(5000)).toBe(5);
    expect(gbToMb(0.5)).toBe(500);
    expect(cacheSizeProblem(5)).toBe("");
    expect(cacheSizeProblem(0.01)).not.toBe("");
    expect(cacheSizeProblem(Number.NaN)).not.toBe("");
    expect(cacheSizeProblem(99999)).not.toBe("");
  });

  it("treats a region from an older server as built, and sizes on-demand terrain small", () => {
    expect(terrainMode({})).toBe("prebuilt");
    expect(terrainMode({ terrain_mode: "on_demand" })).toBe("on_demand");
    expect(onDemandTerrainBytes(4e9)).toBe(4e7);
  });
});

describe("on-demand terrain in the pages", () => {
  it("Overview shows cache use, the cache hit rate and the sources' health", async () => {
    renderApp("/");
    const card = (await screen.findByRole("heading", { name: /On-demand terrain/ })).closest(
      "section",
    ) as HTMLElement;
    expect(within(card).getByTestId("ondemand-cache-use")).toHaveTextContent("1.3 GB of 5.0 GB");
    expect(within(card).getByText(/93% from the cache/)).toBeInTheDocument();
    expect(within(card).getAllByText("Answering")).toHaveLength(2);
    expect(within(card).getByText(/Applies to Alberta, at zooms 9 to 13/)).toBeInTheDocument();
    // The disk breakdown counts the cache.
    expect(screen.getByText("On-demand terrain cache")).toBeInTheDocument();
  });

  it("Overview says which source stopped answering", async () => {
    renderApp("/", { authed: true, onDemandFailing: true });
    const card = (await screen.findByRole("heading", { name: /On-demand terrain/ })).closest(
      "section",
    ) as HTMLElement;
    expect(within(card).getByText("a source is not answering")).toBeInTheDocument();
    expect(within(card).getByText("Not answering")).toBeInTheDocument();
    expect(within(card).getByText(/answered 503/)).toBeInTheDocument();
  });

  it("Overview says nothing about it on a server that does not use it", async () => {
    renderApp("/", { authed: true, empty: true });
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    await screen.findByText("Processes");
    expect(screen.queryByRole("heading", { name: /On-demand terrain/ })).not.toBeInTheDocument();
  });

  it("Settings explains the trade-off and changes the two settings", async () => {
    const { user } = renderApp("/settings");
    const card = (await screen.findByRole("heading", { name: /On-demand terrain/ })).closest(
      "section",
    ) as HTMLElement;
    const c = within(card);
    // The trade-off, in words: latency, internet, disk; and what it does not cover.
    expect(c.getByText(/waits about a second/)).toBeInTheDocument();
    expect(c.getByText(/needs the internet/)).toBeInTheDocument();
    expect(c.getByText(/Map tiles and\s+routing are always built/)).toBeInTheDocument();
    expect(c.getByText("Alberta")).toBeInTheDocument(); // the region set to on demand

    // Outside built regions: off → on demand, saved at once.
    const outside = c.getByRole("radiogroup", { name: "Terrain outside built regions" });
    expect(within(outside).getByRole("radio", { name: "Off" })).toBeChecked();
    await user.click(within(outside).getByRole("radio", { name: "On demand" }));
    await waitFor(() =>
      expect(within(outside).getByRole("radio", { name: "On demand" })).toBeChecked(),
    );
    expect(c.getByText(/Any place on Earth has terrain/)).toBeInTheDocument();

    // Cache size: Save is offered only for a change that can be saved.
    const size = c.getByLabelText("Cache size");
    const save = c.getByRole("button", { name: "Save" });
    expect(size).toHaveValue(5);
    expect(save).toBeDisabled();
    await user.clear(size);
    await user.type(size, "0.01");
    expect(save).toBeDisabled();
    expect(c.getByText(/At least 64 MB/)).toBeInTheDocument();
    await user.clear(size);
    await user.type(size, "2");
    expect(save).toBeEnabled();
    await user.click(save);
    await waitFor(() =>
      expect(c.getByTestId("ondemand-cache-use")).toHaveTextContent("1.3 GB of 2.0 GB"),
    );
    expect(save).toBeDisabled();

    // Clearing asks first, then empties the cache.
    await user.click(c.getByRole("button", { name: "Clear the cache" }));
    const dialog = await screen.findByRole("dialog", { name: "Clear the on-demand cache?" });
    expect(within(dialog).getByText(/Built terrain is not touched/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(c.getByText(/0 files/)).toBeInTheDocument());
    expect(c.getByRole("button", { name: "Clear the cache" })).toBeDisabled();
  });

  it("Regions shows each region's terrain and changes it", async () => {
    const { user } = renderApp("/regions");
    const texas = (await screen.findByText("Texas")).closest("tr") as HTMLElement;
    const alberta = screen.getByText("Alberta").closest("tr") as HTMLElement;
    expect(within(alberta).getByRole("button", { name: /Terrain read on demand/ })).toBeVisible();
    await user.click(within(texas).getByRole("button", { name: /Terrain built/ }));

    const dialog = await screen.findByRole("dialog", { name: "Terrain for Texas" });
    const d = within(dialog);
    expect(d.getByRole("radio", { name: /Build it/ })).toBeChecked();
    expect(d.getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(d.getByRole("radio", { name: /Read on demand/ }));
    expect(d.getByText(/keeps being served until the next build/)).toBeInTheDocument();
    await user.click(d.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(within(texas).getByRole("button", { name: /Terrain read on demand/ })).toBeVisible(),
    );
  });

  it("a new region can be added with its terrain on demand", async () => {
    const { user } = renderApp("/regions?add=1");
    await user.type(await screen.findByLabelText("Search regions"), "dela");
    await user.click(await screen.findByRole("button", { name: /^Delaware/ }));
    const est = await screen.findByTestId("estimate");
    const built = within(est).getByText("Terrain").nextElementSibling?.textContent;
    await user.click(within(est).getByRole("radio", { name: "Read on demand" }));
    // The estimate follows: almost no terrain disk.
    await waitFor(() =>
      expect(within(est).getAllByText("Terrain")[0].nextElementSibling?.textContent).not.toBe(
        built,
      ),
    );
    await user.click(screen.getByRole("button", { name: "Add only" }));
    const row = await screen.findByRole("row", { name: /Delaware/ });
    expect(
      await within(row).findByRole("button", { name: /Terrain read on demand/ }),
    ).toBeVisible();
  });
});
