// Loads every style in real mapbox-gl 1.13.1 and current MapLibre GL JS in headless Chromium,
// fails on any style/runtime error, toggles the "satellite" layer, and saves screenshots.
// Not shipped. Needs the devserver running and Playwright's Chromium in ~/.cache/ms-playwright.
//
//   bun docker/renderer/dev/glcheck.js [devserver-base] [out-dir]
import { chromium } from "playwright-core";
import { readdirSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const BASE = process.argv[2] || "http://127.0.0.1:18080";
const OUT = process.argv[3] || "data/screens/gl";
mkdirSync(OUT, { recursive: true });

const LIBS = {
  "mapbox-gl-1.13.1": {
    js: "https://unpkg.com/mapbox-gl@1.13.1/dist/mapbox-gl.js",
    css: "https://unpkg.com/mapbox-gl@1.13.1/dist/mapbox-gl.css",
    ns: "mapboxgl",
  },
  "maplibre-gl-6.17.0": {
    js: "https://unpkg.com/maplibre-gl@6.17.0/dist/maplibre-gl.mjs",
    esm: true, // MapLibre 6 ships only an ES module build
    css: "https://unpkg.com/maplibre-gl@6.17.0/dist/maplibre-gl.css",
    ns: "maplibregl",
  },
};
const STYLES = ["outdoors", "outdoors-dark", "satellite"];
const VIEW = { center: [-102.22, 31.92], zoom: 13 };

function findChromium() {
  const root = join(homedir(), ".cache/ms-playwright");
  const dirs = readdirSync(root).filter((d) => /^chromium-\d+$/.test(d)).sort();
  if (!dirs.length) throw new Error("no Playwright chromium in ~/.cache/ms-playwright");
  return join(root, dirs.at(-1), "chrome-linux64/chrome");
}

const browser = await chromium.launch({
  executablePath: findChromium(),
  args: ["--use-angle=swiftshader", "--enable-unsafe-swiftshader", "--ignore-gpu-blocklist"],
});
let failed = 0;
for (const [libName, lib] of Object.entries(LIBS)) {
  for (const style of STYLES) {
    const page = await browser.newPage({ viewport: { width: 900, height: 700 } });
    const errors = [];
    page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
    await page.setContent(`<!doctype html><html><head><link rel="stylesheet" href="${lib.css}">
      <style>html,body,#m{margin:0;height:100%}</style>${lib.esm ? `<script type="module">import * as gl from "${lib.js}"; window.${lib.ns} = gl;</script>` : `<script src="${lib.js}"></script>`}</head>
      <body><div id="m"></div></body></html>`);
    const result = await page.evaluate(
      async ({ ns, url, view }) => {
        for (let i = 0; i < 300 && !window[ns]; i++) await new Promise((r) => setTimeout(r, 100));
        const gl = window[ns];
        if (!gl) return { errs: [`${ns} did not load`], info: { satellite: false } };
        const errs = [];
        const map = new gl.Map({ container: "m", style: url, center: view.center, zoom: view.zoom, fadeDuration: 0 });
        map.on("error", (e) => errs.push(String(e.error?.message || e.error || e)));
        const idle = () => new Promise((r) => map.once("idle", r));
        await Promise.race([idle(), new Promise((r) => setTimeout(r, 30000))]);
        const style = map.getStyle();
        const sat = map.getLayer("satellite");
        const info = { layers: style.layers.length, satellite: !!sat, satVisibility: map.getLayoutProperty("satellite", "visibility") || "visible" };
        // the client apps toggle this layer: make sure that works
        map.setLayoutProperty("satellite", "visibility", info.satVisibility === "none" ? "visible" : "none");
        await Promise.race([idle(), new Promise((r) => setTimeout(r, 30000))]);
        info.toggled = map.getLayoutProperty("satellite", "visibility");
        map.setLayoutProperty("satellite", "visibility", info.satVisibility);
        await Promise.race([idle(), new Promise((r) => setTimeout(r, 30000))]);
        return { errs, info };
      },
      { ns: lib.ns, url: `${BASE}/styles/${style}.json`, view: VIEW },
    );
    // 404s for resources outside the dev extracts are not style errors
    const real = [...errors, ...result.errs].filter((e) => !/\b(404|204)\b|Not Found/i.test(e));
    const file = `${OUT}/${libName}-${style}.png`;
    await page.screenshot({ path: file });
    const ok = real.length === 0 && result.info.satellite;
    if (!ok) failed++;
    console.log(`${ok ? "ok  " : "FAIL"} ${libName} ${style}: ${JSON.stringify(result.info)} ${real.length ? real.join(" | ") : ""} -> ${file}`);
    await page.close();
  }
}
await browser.close();
process.exit(failed ? 1 : 0);
