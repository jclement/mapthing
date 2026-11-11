import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Overview, ProcessInfo } from "../api";
import {
  diskShares,
  headline,
  latency,
  logLink,
  needsAttention,
  processLabel,
  rate,
  totals,
  unionBBox,
} from "../lib/overview";
import { renderApp } from "./util";

const proc = (over: Partial<ProcessInfo>): ProcessInfo => ({
  name: "renderer",
  state: "running",
  healthy: true,
  pid: 5,
  uptime_s: 100,
  restarts: 0,
  rss_bytes: 1,
  cpu_percent: 1,
  ...over,
});

const base = (over: Partial<Overview> = {}): Overview => ({
  version: "1",
  commit: "",
  started_at: "2026-11-11T00:00:00Z",
  ready: true,
  readiness: [],
  generation: { id: "g1", bytes: 1, regions: [] },
  regions_installed: 1,
  processes: [proc({ name: "mapthing" }), proc({})],
  system: {
    cpus: 4,
    load1: 0,
    memory_used_bytes: 1,
    memory_total_bytes: 2,
    platform: "linux/amd64",
  },
  traffic: { since: "2026-11-11T00:00:00Z", kinds: [] },
  disk: {
    total_bytes: 100,
    free_bytes: 30,
    data_bytes: 50,
    parts: [],
    guard: { known: true, need_bytes: 20, ok: true },
  },
  builds: { schedule_enabled: true, cron: "0 3 * * 0", next_run: null, active: null, last: null },
  ...over,
});

describe("overview verdict", () => {
  it("says Serving only when nothing needs attention", () => {
    expect(headline(base())).toMatchObject({ tone: "ok", title: "Serving" });
  });

  it("walks a new install through its first steps", () => {
    const fresh = base({ generation: null, regions_installed: 0, ready: false });
    expect(headline(fresh)).toMatchObject({ tone: "info", title: "Nothing to serve yet" });
    expect(headline({ ...fresh, regions_installed: 2 }).title).toBe(
      "Regions are chosen but not built",
    );
    const building = { ...fresh, regions_installed: 2 };
    building.builds = { ...building.builds, active: { id: "1" } as never };
    expect(headline(building).title).toBe("Building the first map");
  });

  it("puts a crashed or unresponsive process first", () => {
    const o = base({ processes: [proc({ name: "mapthing" }), proc({ state: "restarting" })] });
    expect(headline(o)).toMatchObject({ tone: "bad", title: "renderer needs attention" });
    expect(needsAttention(proc({ healthy: false }))).toBe(true);
    // a standby warming up during a swap is expected to be unhealthy for a moment
    expect(needsAttention(proc({ healthy: false, standby: true }))).toBe(false);
    expect(processLabel(proc({ healthy: false, standby: true }))).toBe("warming up");
    expect(processLabel(proc({ state: "restarting" }))).toBe("crashed, restarting");
  });

  it("warns about a failed build and a build that will not fit, while still serving", () => {
    const failed = base();
    failed.builds = { ...failed.builds, last: { id: "9", status: "failed", trigger: "schedule" } };
    expect(headline(failed)).toMatchObject({ tone: "warn" });
    expect(headline(failed).title).toMatch(/last build failed/);
    const full = base();
    full.disk = { ...full.disk, guard: { known: true, need_bytes: 40, ok: false } };
    expect(headline(full).title).toMatch(/will not fit/);
  });

  it("links a crashed child straight to its lines, as far back as they go", () => {
    expect(logLink(proc({ name: "valhalla@8002", restarts: 2 }))).toBe(
      "/logs?source=valhalla%408002&range=all",
    );
    expect(logLink(proc({ name: "renderer" }))).toBe("/logs?source=renderer");
    expect(logLink(proc({ name: "build tools" }))).toBe("/logs?source=build");
  });
});

describe("overview numbers", () => {
  it("formats latency and rates for reading", () => {
    expect(latency(0)).toBe("—");
    expect(latency(0.42)).toBe("0.4 ms");
    expect(latency(38.4)).toBe("38 ms");
    expect(latency(1250)).toBe("1.3 s");
    expect(rate(0)).toBe("0");
    expect(rate(0.5)).toBe("0.50/min");
    expect(rate(42)).toBe("42/min");
    expect(rate(300)).toBe("5.0/s");
  });

  it("totals traffic across kinds", () => {
    const w = (requests: number, errors = 0) => ({
      requests,
      errors,
      rejected: 0,
      per_min: requests / 60,
      p50_ms: 1,
      p95_ms: 2,
    });
    const t = totals([
      { kind: "tiles", hour: w(60), day: w(600, 2), series: [1, 2] },
      { kind: "route", hour: w(30, 1), day: w(100), series: [3, 4] },
    ]);
    expect(t).toMatchObject({ hour: 90, day: 700, errorsHour: 1, errorsDay: 2, perMin: 1.5 });
    expect(t.series.slice(0, 2)).toEqual([4, 6]);
  });

  it("splits the disk into mapthing, other and free, with the build guard mark", () => {
    const s = diskShares(base().disk);
    expect(s).toMatchObject({ data: 0.5, other: 0.2, free: 0.3, otherBytes: 20, need: 0.2 });
    expect(diskShares({ ...base().disk, guard: { known: false } }).need).toBeNull();
  });

  it("frames every region", () => {
    expect(unionBBox([{ bbox: [-104, 31, -101, 33] }, { bbox: [-120, 49, -110, 60] }, {}])).toEqual(
      [-120, 31, -101, 60],
    );
    expect(unionBBox([{}])).toBeUndefined();
  });
});

describe("Overview page", () => {
  it("is the admin home: state, processes, traffic, resources and builds", async () => {
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    expect(await screen.findByRole("status")).toHaveTextContent("Serving");
    // live generation and its regions
    expect(screen.getByRole("link", { name: "20261101-030012" })).toHaveAttribute(
      "href",
      "/generations",
    );
    expect(screen.getByText("Permian core")).toBeInTheDocument();
    // every process, with restarts and a way to its log
    const procs = within(
      screen.getByRole("heading", { name: "Processes" }).closest("section") as HTMLElement,
    );
    for (const name of ["mapthing", "renderer", "valhalla@8003"])
      expect(procs.getByText(name)).toBeInTheDocument();
    expect(screen.getByText("1 restart")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Its log, with the restarts/ })).toHaveAttribute(
      "href",
      "/logs?source=renderer&range=all",
    );
    // traffic by kind with latency
    const table = screen.getByRole("table");
    for (const kind of ["Vector tiles", "Raster tiles", "Routing", "Elevation", "Offline packs"])
      expect(within(table).getByText(kind)).toBeInTheDocument();
    expect(within(table).getAllByText(/ms|s$/).length).toBeGreaterThan(6);
    // resources and builds
    expect(screen.getByText(/The next build needs about/)).toBeInTheDocument();
    expect(screen.getByText("Next scheduled")).toBeInTheDocument();
    expect(screen.getByText("Last build")).toBeInTheDocument();
  });

  it("first run: says what to do next", async () => {
    renderApp("/", { authed: true, empty: true });
    expect(await screen.findByRole("status")).toHaveTextContent("Nothing to serve yet");
    expect(screen.getAllByRole("link", { name: /Add a region/ })[0]).toHaveAttribute(
      "href",
      "/regions?add=1",
    );
    expect(screen.getByText("No map is live")).toBeInTheDocument();
    expect(screen.getByText(/No requests since the server started/)).toBeInTheDocument();
    expect(screen.getByText(/disk check runs once regions are chosen/)).toBeInTheDocument();
    expect(screen.getByText("None yet")).toBeInTheDocument();
  });

  it("says when a newer image exists and how to update", async () => {
    renderApp("/", { authed: true, update: "available" });
    const note = await screen.findByRole("region", { name: "Software update" });
    expect(note).toHaveTextContent("A newer mapthing image is available");
    expect(note).toHaveTextContent("revision c0ffee123456; this server runs 7efee20a1b2c");
    expect(note).toHaveTextContent("docker compose pull && docker compose up -d");
    expect(note).toHaveTextContent("Your maps, keys and settings are kept");
    expect(note).toHaveTextContent("No build is running");
    expect(
      within(note).getByRole("button", { name: "Copy the update command" }),
    ).toBeInTheDocument();
  });

  it("is quiet when up to date, and says so when the check is off", async () => {
    const first = renderApp("/");
    expect(await screen.findByText(/up to date \(checked 3 h ago\)/)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Software update" })).not.toBeInTheDocument();
    first.unmount();
    renderApp("/", { authed: true, update: "off" });
    expect(await screen.findByText(/update check off: turned off/)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Software update" })).not.toBeInTheDocument();
  });
});
