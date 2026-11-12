// Screenshots of on-demand terrain in the admin UI, against the mock backend
// (npm run dev:mock must be running): every place it shows, at desktop and phone width, in
// both themes.
// Usage: node scripts/screens-ondemand.mjs [baseURL] [outDir]
import { mkdirSync } from "node:fs";
import { chromium } from "playwright";

const base = process.argv[2] ?? "http://localhost:5181/admin";
const out = process.argv[3] ?? new URL("../../data/screens-ondemand/", import.meta.url).pathname;
mkdirSync(out, { recursive: true });

const browser = await chromium.launch({
  args: ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader"],
});

const sizes = [
  { tag: "1280", width: 1280, height: 900 },
  { tag: "400", width: 400, height: 860 },
];

async function shot(name, path, { before, target, fullPage = false } = {}) {
  for (const size of sizes) {
    for (const dark of [false, true]) {
      const ctx = await browser.newContext({
        viewport: { width: size.width, height: size.height },
        colorScheme: dark ? "dark" : "light",
        deviceScaleFactor: 1,
      });
      const page = await ctx.newPage();
      page.on("pageerror", (e) => console.error(`[${name}] pageerror:`, e.message));
      await page.goto(base + path, { waitUntil: "networkidle" });
      await page.waitForTimeout(900);
      if (before) await before(page);
      if (target) {
        // Bring the part under test into view; phones scroll a long way to reach it.
        await target(page).scrollIntoViewIfNeeded();
        await page.waitForTimeout(200);
      }
      const file = `${out}/${name}-${size.tag}-${dark ? "dark" : "light"}.png`;
      await page.screenshot({ path: file, fullPage });
      console.log("wrote", file);
      await ctx.close();
    }
  }
}

const card = (page) =>
  page.getByRole("heading", { name: /On-demand terrain/ }).locator("xpath=ancestor::section[1]");

await shot("01-overview", "/", { target: card });
await shot("02-overview-source-down", "/?odfail", { target: card });
await shot("03-settings", "/settings", { target: card });
await shot("04-settings-full", "/settings", { fullPage: true });
await shot("05-settings-outside-on", "/settings", {
  target: card,
  before: async (page) => {
    await page
      .getByRole("radiogroup", { name: "Terrain outside built regions" })
      .getByRole("radio", { name: "On demand" })
      .click();
    await page.getByLabel("Cache size").fill("0.01");
    await page.waitForTimeout(300);
  },
});
await shot("06-settings-clear", "/settings", {
  before: async (page) => {
    await page.getByRole("button", { name: "Clear the cache" }).click();
    await page.waitForTimeout(300);
  },
});
await shot("07-regions", "/regions");
await shot("08-regions-terrain-dialog", "/regions", {
  before: async (page) => {
    await page
      .getByRole("button", { name: /Terrain built/ })
      .first()
      .click();
    await page.getByRole("radio", { name: /Read on demand/ }).click();
    await page.waitForTimeout(300);
  },
});
await shot("09-regions-add", "/regions?add=1", {
  before: async (page) => {
    await page.getByLabel("Search regions").fill("dela");
    await page.getByRole("button", { name: /^Delaware/ }).click();
    await page.getByRole("radio", { name: "Read on demand" }).click();
    await page.waitForTimeout(300);
    await page.getByTestId("estimate").scrollIntoViewIfNeeded();
  },
});

const warm = (page) => page.getByText("Warm an area for offline use");
await shot("10-settings-warm-too-big", "/settings", { target: warm });
await shot("11-settings-warm-done", "/settings", {
  target: warm,
  before: async (page) => {
    await page.getByLabel("Down to zoom").selectOption("10");
    await page.waitForTimeout(700);
    await page.getByRole("button", { name: "Warm this area" }).click();
    await page.waitForTimeout(4500);
  },
});

await browser.close();
