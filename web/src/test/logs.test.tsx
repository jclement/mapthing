import { act, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { LogEntry } from "../api";
import {
  apiQuery,
  compareIds,
  EMPTY_FILTER,
  filterToSearch,
  follows,
  mergeEntries,
  parseFilter,
  timeWindow,
} from "../lib/logs";
import { emitLog } from "../mocks/server";
import { renderApp } from "./util";

const e = (id: string, msg = id): LogEntry => ({
  id,
  t: "2026-11-11T12:00:00Z",
  source: "mapthing",
  level: "info",
  msg,
});

describe("log filter", () => {
  it("round-trips through the page URL, leaving defaults out", () => {
    expect(filterToSearch(EMPTY_FILTER).toString()).toBe("");
    const f = parseFilter(new URLSearchParams("source=renderer,xvfb&level=warn&q=heap&range=24h"));
    expect(f).toMatchObject({
      sources: ["renderer", "xvfb"],
      level: "warn",
      q: "heap",
      range: "24h",
    });
    expect(parseFilter(filterToSearch(f))).toEqual(f);
  });

  it("a link to a build's lines shows the whole build, not just the latest lines", () => {
    const f = parseFilter(new URLSearchParams("job=12"));
    expect(f).toMatchObject({ job: "12", range: "all" });
    expect(filterToSearch(f).toString()).toBe("job=12");
  });

  it("ignores junk in the URL", () => {
    const f = parseFilter(new URLSearchParams("level=loud&range=forever&since=yesterday"));
    expect(f).toEqual(EMPTY_FILTER);
  });

  it("a since or until in the URL means a custom range", () => {
    const f = parseFilter(new URLSearchParams("since=2026-11-10T00:00:00Z"));
    expect(f.range).toBe("custom");
    expect(f.since).toBe("2026-11-10T00:00:00.000Z");
    expect(follows(f)).toBe(true); // no end: lines keep arriving
    const past = { ...f, until: "2026-11-10T06:00:00.000Z" };
    expect(follows(past, Date.parse("2026-11-11T00:00:00Z"))).toBe(false);
  });

  it("builds the API query: relative ranges resolve against now", () => {
    const now = Date.parse("2026-11-11T12:00:00Z");
    expect(timeWindow({ ...EMPTY_FILTER, range: "1h" }, now)).toEqual({
      since: "2026-11-11T11:00:00.000Z",
    });
    expect(apiQuery(EMPTY_FILTER)).toBe("");
    const q = apiQuery(
      { ...EMPTY_FILTER, sources: ["build"], level: "error", q: " oom ", job: "7", range: "24h" },
      { before: "42", limit: "500" },
      now,
    );
    expect(Object.fromEntries(new URLSearchParams(q))).toEqual({
      source: "build",
      level: "error",
      q: "oom",
      job: "7",
      since: "2026-11-10T12:00:00.000Z",
      before: "42",
      limit: "500",
    });
  });
});

describe("merging log lines", () => {
  it("compares ids as numbers, whatever their length", () => {
    expect(compareIds("999", "1000")).toBeLessThan(0);
    expect(compareIds("1762862400000000001", "1762862400000000000")).toBeGreaterThan(0);
    expect(compareIds("5", "5")).toBe(0);
  });

  it("appends a live batch and drops the oldest past the cap", () => {
    const out = mergeEntries([e("1"), e("2")], [e("3"), e("4")], 3);
    expect(out.map((x) => x.id)).toEqual(["2", "3", "4"]);
  });

  it("skips lines already shown (the stream replays the gap)", () => {
    const out = mergeEntries([e("1"), e("2"), e("3")], [e("2"), e("3"), e("4")], 10);
    expect(out.map((x) => x.id)).toEqual(["1", "2", "3", "4"]);
  });

  it("puts an older page in front and keeps it when over the cap", () => {
    const out = mergeEntries([e("5"), e("6")], [e("3"), e("4")], 3, "old");
    expect(out.map((x) => x.id)).toEqual(["3", "4", "5"]);
  });
});

describe("Logs page", () => {
  it("shows history, filters by source and level, and tails new lines", async () => {
    const { user } = renderApp("/logs");
    const log = await screen.findByRole("log", { name: "Server log" });
    expect(await within(log).findByText(/Geofabrik index refreshed/)).toBeInTheDocument();
    expect(screen.getByText(/back to/)).toBeInTheDocument();

    // live tail
    act(() => {
      emitLog({ source: "renderer", level: "error", msg: "boom: render failed" });
    });
    expect(await within(log).findByText("boom: render failed")).toBeInTheDocument();

    // source filter: only the renderer's lines stay
    await user.click(screen.getByRole("button", { name: "renderer" }));
    await waitFor(() =>
      expect(within(log).queryByText(/Geofabrik index refreshed/)).not.toBeInTheDocument(),
    );
    expect(await within(log).findByText("boom: render failed")).toBeInTheDocument();
    expect(within(log).getByText(/renderer listening/)).toBeInTheDocument();

    // level filter on top of it
    await user.click(screen.getByRole("radio", { name: "Error" }));
    await waitFor(() =>
      expect(within(log).queryByText(/renderer listening/)).not.toBeInTheDocument(),
    );
    expect(within(log).getByText(/heap out of memory/)).toBeInTheDocument();

    // lines from another source do not appear in this filtered tail
    act(() => {
      emitLog({ source: "mapthing", level: "error", msg: "not for this view" });
    });
    act(() => {
      emitLog({ source: "renderer", level: "error", msg: "second failure" });
    });
    expect(await within(log).findByText("second failure")).toBeInTheDocument();
    expect(within(log).queryByText("not for this view")).not.toBeInTheDocument();
  });

  it("pauses the tail, counts what arrives, and shows it on resume", async () => {
    const { user } = renderApp("/logs");
    const log = await screen.findByRole("log", { name: "Server log" });
    await within(log).findByText(/Geofabrik index refreshed/);
    await user.click(screen.getByRole("button", { name: "Pause" }));
    act(() => {
      emitLog({ source: "mapthing", level: "info", msg: "while paused" });
    });
    const resume = await screen.findByRole("button", { name: "Resume (1 new)" });
    expect(within(log).queryByText("while paused")).not.toBeInTheDocument();
    await user.click(resume);
    expect(await within(log).findByText("while paused")).toBeInTheDocument();
  });

  it("opens on one build's lines from a link, and says so when nothing matches", async () => {
    const { user } = renderApp("/logs?job=j_0102");
    const log = await screen.findByRole("log", { name: "Server log" });
    expect(await within(log).findByText(/OutOfMemoryError/)).toBeInTheDocument();
    expect(within(log).queryByText(/Geofabrik index refreshed/)).not.toBeInTheDocument();
    expect(screen.getByText("Build #j_0102")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Download/ })).toHaveAttribute(
      "href",
      "/admin/api/logs/download?job=j_0102",
    );

    await user.type(screen.getByLabelText("Search log text"), "no such text anywhere");
    expect(await within(log).findByText("No lines match")).toBeInTheDocument();
  });

  it("links a failed build to its log lines", async () => {
    const { user } = renderApp("/builds");
    const failed = await screen.findByText("Failed");
    await user.click(failed);
    expect(await screen.findByRole("link", { name: /went wrong in the log/ })).toHaveAttribute(
      "href",
      expect.stringContaining("/logs?job="),
    );
  });
});
