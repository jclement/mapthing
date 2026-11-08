import { describe, expect, test } from "bun:test";
import { parseRenderPath, rewriteURL, tileCenter } from "./lib.js";

describe("parseRenderPath", () => {
  const cases = [
    ["/render/outdoors/512/12/917/1663.png", { style: "outdoors", size: 512, z: 12, x: 917, y: 1663, ratio: 1, format: "png" }],
    ["/render/outdoors-dark/256/0/0/0@2x.png", { style: "outdoors-dark", size: 256, z: 0, x: 0, y: 0, ratio: 2, format: "png" }],
    ["/render/satellite/256/3/7/7.png", { style: "satellite", size: 256, z: 3, x: 7, y: 7, ratio: 1, format: "png" }],
    ["/render/outdoors/300/1/0/0.png", null],
    ["/render/outdoors/512/1/2/0.png", null],
    ["/render/../512/1/0/0.png", null],
    ["/render/Outdoors/512/1/0/0.png", null],
    ["/render/outdoors/512/23/0/0.png", null],
    ["/render/outdoors/512/1/0/0@3x.png", null],
    ["/render/satellite/512/1/0/0@2x.jpg", { style: "satellite", size: 512, z: 1, x: 0, y: 0, ratio: 2, format: "jpg" }],
    ["/render/outdoors/256/1/1/1.webp", { style: "outdoors", size: 256, z: 1, x: 1, y: 1, ratio: 1, format: "webp" }],
    ["/render/outdoors/512/1/0/0.jpeg", null],
    ["/render/outdoors/512/1/0/0.gif", null],
  ];
  for (const [path, want] of cases) test(path, () => expect(parseRenderPath(path)).toEqual(want));
});

test("tileCenter", () => {
  expect(tileCenter(0, 0, 0)).toEqual([0, 0]);
  const [lon, lat] = tileCenter(1, 0, 0);
  expect(lon).toBe(-90);
  expect(lat).toBeCloseTo(66.513, 2);
});

describe("rewriteURL", () => {
  const base = "http://127.0.0.1:8080";
  test("public host -> internal base", () => {
    expect(rewriteURL("https://maps.example.com/tiles/basemap/1/0/0.mvt", base)).toBe("http://127.0.0.1:8080/tiles/basemap/1/0/0.mvt");
  });
  test("rewrites by path, leaves other paths alone", () => {
    expect(rewriteURL("https://example.org/tiles/x.png", base)).toBe("http://127.0.0.1:8080/tiles/x.png");
    expect(rewriteURL("https://example.org/other/x.png", base)).toBe("https://example.org/other/x.png");
  });
  test("sprite suffix after query", () => {
    expect(rewriteURL("http://h/sprites/outdoors?key=mt_1@2x.json", base)).toBe("http://127.0.0.1:8080/sprites/outdoors@2x.json?key=mt_1");
    expect(rewriteURL("http://h/sprites/outdoors?key=mt_1.png", base)).toBe("http://127.0.0.1:8080/sprites/outdoors.png?key=mt_1");
  });
  test("glyph url with spaces", () => {
    expect(rewriteURL("http://h/fonts/Noto%20Sans%20Regular/0-255.pbf", base)).toBe("http://127.0.0.1:8080/fonts/Noto%20Sans%20Regular/0-255.pbf");
  });
});
