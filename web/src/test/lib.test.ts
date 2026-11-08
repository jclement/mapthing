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
