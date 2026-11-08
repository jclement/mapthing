// Screenshots of every page against the mock backend (npm run dev:mock must be running).
// Usage: node scripts/screens.mjs [baseURL] [outDir]
import { mkdirSync } from "node:fs";
import { chromium } from "playwright";

const base = process.argv[2] ?? "http://127.0.0.1:5181/admin";
const out = process.argv[3] ?? new URL("../../data/screens/", import.meta.url).pathname;
mkdirSync(out, { recursive: true });

const browser = await chromium.launch({
  args: ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader"],
});

async function shot(name, path, { width = 1440, height = 900, dark = false, before } = {}) {
  const ctx = await browser.newContext({
    viewport: { width, height },
    colorScheme: dark ? "dark" : "light",
    deviceScaleFactor: 1,
  });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => console.error(`[${name}] pageerror:`, e.message));
  await page.goto(base + path, { waitUntil: "networkidle" });
  await page.waitForTimeout(1200);
  if (before) await before(page);
  await page.screenshot({ path: `${out}/${name}.png` });
  console.log("wrote", `${out}/${name}.png`);
  await ctx.close();
}

const focusPermian = (page) =>
  page.evaluate(() => window.__map.jumpTo({ center: [-102.75, 31.95], zoom: 9.5 }));

const clickMap = async (page, pts) => {
  const box = await page.locator(".maplibregl-canvas").boundingBox();
  for (const [fx, fy] of pts) {
    await page.mouse.click(box.x + box.width * fx, box.y + box.height * fy);
    await page.waitForTimeout(600);
  }
};

await shot("01-login", "/?login");
await shot("02-first-run-empty", "/?empty");
await shot("03-map", "/");
await shot("04-map-route-profile", "/", {
  before: async (page) => {
    await focusPermian(page);
    await page.waitForTimeout(1500);
    await page.getByRole("radio", { name: /Route/ }).click();
    await clickMap(page, [
      [0.42, 0.62],
      [0.58, 0.48],
    ]);
    await page.waitForTimeout(1200);
    const chart = page.getByRole("img", { name: "Elevation profile" });
    const b = await chart.boundingBox();
    if (b) await page.mouse.move(b.x + b.width * 0.6, b.y + b.height / 2);
    await page.waitForTimeout(300);
  },
});
await shot("05-map-style-picker", "/", {
  before: async (page) => {
    await page.getByRole("button", { name: "Map style" }).click();
    await page.waitForTimeout(300);
  },
});
await shot("06-map-dark-elevation", "/", {
  dark: true,
  before: async (page) => {
    await focusPermian(page);
    await page.waitForTimeout(1500);
    await clickMap(page, [[0.5, 0.55]]);
    await page.waitForTimeout(500);
  },
});
await shot("07-regions", "/regions");
await shot("08-regions-add-catalog", "/regions?empty&add=1", {
  before: async (page) => {
    await page.getByLabel("Search regions").fill("tex");
    await page.getByRole("button", { name: /^Texas/ }).click();
    await page.waitForTimeout(300);
  },
});
await shot("09-regions-add-draw", "/regions?add=1", {
  before: async (page) => {
    await page.getByRole("radio", { name: /Draw a box/ }).click();
    await page.getByLabel("Name").fill("Permian north");
    await page.getByLabel(/Bounding box/).fill("-103.8, 31.9, -102.1, 33.1");
    await page.waitForTimeout(1500);
  },
});
await shot("10-builds-running", "/regions?empty&add=1", {
  before: async (page) => {
    await page.getByLabel("Search regions").fill("dela");
    await page.getByRole("button", { name: /^Delaware/ }).click();
    await page.getByRole("button", { name: "Add and build" }).click();
    await page.waitForTimeout(7000);
  },
});
await shot("11-builds-history", "/builds");
await shot("12-schedule", "/schedule");
await shot("13-keys", "/keys");
await shot("14-keys-created", "/keys", {
  before: async (page) => {
    await page.getByRole("button", { name: "Create key" }).first().click();
    await page.getByLabel("Label").fill("Dispatch dashboard");
    await page.getByLabel("Requests per minute").fill("300");
    await page.getByRole("button", { name: "Create key" }).last().click();
    await page.waitForTimeout(600);
  },
});
await shot("15-stats", "/stats");
await shot("16-system", "/system");
await shot("17-stats-dark", "/stats", { dark: true });
await shot("18-mobile-map-route", "/", {
  width: 390,
  height: 844,
  before: async (page) => {
    await focusPermian(page);
    await page.waitForTimeout(1500);
    await page.getByRole("radio", { name: /Route/ }).click();
    await clickMap(page, [
      [0.4, 0.2],
      [0.6, 0.3],
    ]);
    await page.waitForTimeout(1200);
  },
});
await shot("19-mobile-regions", "/regions", { width: 390, height: 844 });
await shot("20-mobile-keys", "/keys", { width: 390, height: 844 });

await browser.close();
