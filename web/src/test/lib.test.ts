import { describe, expect, it } from "vitest";
import { bytes, distance, duration, relTime } from "../lib/format";
import { along, bboxAreaKm2, estimateDisk, haversine, routeFitPadding } from "../lib/geo";

describe("format", () => {
  it.each([
    [0, "0 B"],
    [999, "999 B"],
    [25_000_000, "25.0 MB"],
    [3.9e9, "3.9 GB"],
    [640e6, "640 MB"],
  ])("bytes(%d) = %s", (n, s) => expect(bytes(n)).toBe(s));

  it.each([
    [420, "420 m"],
    [4200, "4.2 km"],
    [42000, "42 km"],
  ])("distance(%d) = %s", (n, s) => expect(distance(n)).toBe(s));

  it.each([
    [30, "30 s"],
    [600, "10 min"],
    [3900, "1 h 5 min"],
  ])("duration(%d) = %s", (n, s) => expect(duration(n)).toBe(s));

  it("relTime", () => {
    const now = Date.parse("2026-11-08T12:00:00Z");
    expect(relTime("2026-11-08T11:00:00Z", now)).toBe("1 h ago");
    expect(relTime("2026-11-05T12:00:00Z", now)).toBe("3 d ago");
    expect(relTime(null, now)).toBe("never");
  });
});

describe("geo", () => {
  it("haversine: one degree of latitude ≈ 111 km", () => {
    expect(haversine([-100, 30], [-100, 31]) / 1000).toBeCloseTo(111.2, 0);
  });
  it("along interpolates", () => {
    const p = along(
      [
        [0, 0],
        [0, 1],
      ],
      haversine([0, 0], [0, 1]) / 2,
    );
    expect(p[1]).toBeCloseTo(0.5, 5);
  });
  it("bboxAreaKm2 measures boxes spanning the whole globe in longitude", () => {
    // Geofabrik's North America extract: −180…180. The old edge-to-edge width made this ~0.
    const na = bboxAreaKm2([-180, 5.57228, 180, 85.04177]);
    expect(na).toBeGreaterThan(2e8);
    expect(na).toBeLessThan(2.6e8);
    // Delaware-sized box: 0.74° × 1.39° at 39°N, about 64 × 155 km.
    const de = bboxAreaKm2([-75.79, 38.45, -75.05, 39.84]);
    expect(de).toBeGreaterThan(9500);
    expect(de).toBeLessThan(10200);
    // A box crossing the antimeridian equals its unwrapped twin.
    expect(bboxAreaKm2([170, 50, -170, 60])).toBeCloseTo(bboxAreaKm2([-10, 50, 10, 60]), 3);
  });
  it("a custom box inside a −180…180 parent gets a sane estimate", () => {
    const parentArea = bboxAreaKm2([-180, 5.57228, 180, 85.04177]);
    const density = 19546581701 / parentArea; // North America extract, bytes per km²
    const est = estimateDisk({ areaKm2: 868, extractBytes: density * 868, bestResM: 10 });
    expect(est.basemap).toBeLessThan(1e9);
    expect(est.routing).toBeLessThan(1e9);
  });
  it("estimateDisk adds up and grows with resolution", () => {
    const area = bboxAreaKm2([-75.79, 38.45, -75.05, 39.84]);
    const coarse = estimateDisk({ areaKm2: area, extractBytes: 25e6, bestResM: 30 });
    const fine = estimateDisk({ areaKm2: area, extractBytes: 25e6, bestResM: 10 });
    expect(coarse.total).toBeCloseTo(
      coarse.download + coarse.basemap + coarse.routing + coarse.terrain,
    );
    expect(fine.terrain).toBeGreaterThan(coarse.terrain);
  });
});

describe("routeFitPadding", () => {
  it("pads past the desktop side panel on the left", () => {
    const p = routeFitPadding(
      { width: 1400, height: 900 },
      { left: 12, top: 64, width: 380, height: 700 },
    );
    expect(p).toEqual({ top: 48, right: 48, bottom: 48, left: 12 + 380 + 48 });
  });
  it("pads above the mobile bottom sheet", () => {
    const p = routeFitPadding(
      { width: 390, height: 700 },
      { left: 0, top: 420, width: 390, height: 280 },
    );
    expect(p).toEqual({ top: 48, right: 48, bottom: 700 - 420 + 48, left: 48 });
  });
  it("adds the toolbar inset at the top", () => {
    expect(routeFitPadding({ width: 1400, height: 900 }, null, 48, 56).top).toBe(104);
  });
  it("uses an even margin with no panel", () => {
    expect(routeFitPadding({ width: 800, height: 600 }, null)).toEqual({
      top: 48,
      right: 48,
      bottom: 48,
      left: 48,
    });
  });
  it("always leaves at least 20% of the map for the route", () => {
    const p = routeFitPadding(
      { width: 390, height: 500 },
      { left: 0, top: 100, width: 390, height: 400 },
    );
    expect(p.top + p.bottom).toBeLessThanOrEqual(500 * 0.8);
    expect(p.bottom).toBeGreaterThan(p.top);
  });
});

describe("limits", () => {
  it("turns the form into what the API takes", async () => {
    const { formToInput, EMPTY_FORM, limitsSummary, share } = await import("../lib/limits");
    expect(
      formToInput({
        ...EMPTY_FORM,
        rps: "2.5",
        daily: "1000",
        origins: "https://a.example, https://b.example",
      }),
    ).toEqual({
      input: {
        rps: 2.5,
        burst: null,
        daily_quota: 1000,
        monthly_quota: null,
        ip_rps: null,
        ip_burst: null,
        origins: ["https://a.example", "https://b.example"],
      },
    });
    // empty and 0 both mean no limit, so an edit can remove one
    expect(formToInput({ ...EMPTY_FORM, rps: "0" })).toMatchObject({ input: { rps: null } });
    for (const bad of [
      { rps: "-1" },
      { burst: "2.5", rps: "1" },
      { burst: "5" },
      { ipBurst: "5" },
      { daily: "abc" },
      { daily: "500", monthly: "100" },
    ])
      expect(formToInput({ ...EMPTY_FORM, ...bad })).toHaveProperty("error");
    expect(
      limitsSummary({
        rpm: null,
        rpd: null,
        rps: 0.5,
        burst: 10,
        monthly_quota: 2000000,
        ip_rps: 5,
        origins: [],
      }),
    ).toEqual(["30/min, burst 10", "2,000,000/month", "5/s per client"]);
    expect(limitsSummary({ rpm: null, rpd: null, origins: [] })).toEqual([]);
    expect(share(50, 200)).toBe(0.25);
    expect(share(500, 200)).toBe(1);
    expect(share(5, null)).toBe(0);
  });
});

describe("updateLine", () => {
  it("covers every state of the update check", async () => {
    const { updateLine } = await import("../lib/overview");
    const base = {
      enabled: true,
      image: "ghcr.io/jclement/mapthing:latest",
      current_revision: "a",
      current_created: null,
      checked_at: null,
      latest_revision: "",
      latest_created: null,
      update_available: false,
    };
    expect(updateLine(null)).toBe("");
    expect(updateLine(base)).toBe("not checked for a newer image yet");
    expect(updateLine({ ...base, error: "the registry answered 502" })).toBe(
      "could not check for a newer image",
    );
    expect(
      updateLine({ ...base, checked_at: new Date().toISOString(), latest_revision: "a" }),
    ).toMatch(/^up to date \(checked /);
    expect(updateLine({ ...base, update_available: true, latest_revision: "b" })).toBe(
      "a newer image is available",
    );
    expect(
      updateLine({
        ...base,
        enabled: false,
        reason: "this is a local build, not a published image",
      }),
    ).toBe("update check off: this is a local build, not a published image");
  });
});
