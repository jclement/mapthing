// End-to-end click-through of the admin UI against a REAL mapthing server (no mocks).
// Usage: MAPTHING_URL=http://127.0.0.1:8080 ADMIN_USER=… ADMIN_PASSWORD=… node scripts/e2e.mjs [outDir]
// Exits non-zero when a step fails or the page logs errors / gets 5xx responses.
import { mkdirSync } from "node:fs";
import { chromium } from "playwright";

const base = (process.env.MAPTHING_URL ?? "http://127.0.0.1:8080").replace(/\/$/, "");
const user = process.env.ADMIN_USER ?? "admin";
const pass = process.env.ADMIN_PASSWORD ?? "";
const out = process.argv[2] ?? new URL("../../data/e2e/", import.meta.url).pathname;
mkdirSync(out, { recursive: true });

const browser = await chromium.launch({
  args: ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader"],
});
const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const page = await ctx.newPage();

const problems = [];
const expected = []; // known-acceptable failures (e.g. renderer or routing absent locally)
const allow = (process.env.E2E_ALLOW ?? "").split(",").filter(Boolean);
page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
page.on("console", (m) => {
  // resource failures are reported (and classified) by the response listener below
  if (m.type() === "error" && !m.text().startsWith("Failed to load resource"))
    problems.push(`console: ${m.text()}`);
});
const seen = { style: 0, tiles: 0, glyphs: 0 };
page.on("response", (r) => {
  const u = r.url();
  if (r.status() === 200 && u.startsWith(base)) {
    if (/\/styles\/[^/]+\.json/.test(u)) seen.style++;
    if (u.includes("/tiles/basemap/")) seen.tiles++;
    if (u.includes("/fonts/")) seen.glyphs++;
  }
  if (!u.startsWith(base) || r.status() < 400) return;
  const line = `${r.status()} ${r.request().method()} ${u.slice(base.length)}`;
  if (r.status() === 401 && u.endsWith("/admin/api/system")) return; // session probe before login
  (allow.some((a) => u.includes(a)) ? expected : problems).push(line);
});

const results = [];
async function step(name, fn) {
  const t = Date.now();
  try {
    await fn();
    results.push(`ok    ${name} (${Date.now() - t} ms)`);
  } catch (e) {
    results.push(`FAIL  ${name}: ${e.message.split("\n")[0]}`);
    await page.screenshot({ path: `${out}/fail-${name.replace(/\W+/g, "-")}.png` }).catch(() => {});
    await page.keyboard.press("Escape").catch(() => {}); // don't let an open dialog fail later steps
  }
}
const shot = (n) => page.screenshot({ path: `${out}/${n}.png` });
const nav = async (label) => {
  const before = page.url();
  await page.getByRole("link", { name: label, exact: true }).first().click();
  await page.waitForURL((u) => u.href !== before, { timeout: 5000 }).catch(() => {});
  await page.waitForLoadState("networkidle");
};

await step("redirect / → /admin/", async () => {
  await page.goto(`${base}/`, { waitUntil: "networkidle" });
  if (!page.url().startsWith(`${base}/admin/`)) throw new Error(`landed on ${page.url()}`);
});

await step("login", async () => {
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: /sign in/i }).click();
  await page.locator(".maplibregl-canvas").waitFor({ timeout: 15000 });
  await page.waitForTimeout(2500); // tiles, glyphs, sprites
  await shot("01-map");
});

await step("map style loads tiles", async () => {
  if (!seen.style || !seen.tiles)
    throw new Error(`style ${seen.style}, basemap tiles ${seen.tiles}`);
});

let created = "";
const label = `e2e key ${Date.now()}`;
await step("keys: create (secret shown once)", async () => {
  await nav("API keys");
  await page.getByRole("button", { name: "Create key" }).first().click();
  const dlg = page.getByRole("dialog", { name: "Create API key" });
  await dlg.getByLabel("Label").fill(label);
  await dlg.getByLabel("Requests per minute").fill("120");
  await dlg.getByRole("button", { name: "Create key" }).click();
  created = (await page.getByTestId("secret").textContent()) ?? "";
  if (!/^mt_[a-z0-9]{52}$/.test(created)) throw new Error(`secret ${created}`);
  await shot("02-key-created");
  await page.getByRole("button", { name: "I have saved it" }).click();
  await page.getByText(label).waitFor();
  if ((await page.content()).includes(created)) throw new Error("secret still on page");
});

await step("keys: the new key works on a keyed endpoint", async () => {
  const r = await fetch(`${base}/tiles/basemap.json?key=${created}`);
  if (r.status !== 200) throw new Error(`status ${r.status}`);
});

await step("keys: revoke", async () => {
  await page.getByRole("button", { name: `Revoke ${label}` }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke key" }).click();
  await page.getByRole("button", { name: `Revoke ${label}` }).waitFor({ state: "detached" });
  const r = await fetch(`${base}/tiles/basemap.json?key=${created}`);
  if (r.status !== 401) throw new Error(`revoked key answered ${r.status}`);
});

await step("regions: add a custom box", async () => {
  await nav("Regions");
  await page.goto(`${base}/admin/regions?add=1`, { waitUntil: "networkidle" });
  const dlg = page.getByRole("dialog", { name: "Add a region" });
  await dlg.getByRole("radio", { name: /Draw a box/ }).click();
  await dlg.getByLabel("Name").fill("E2E box");
  await dlg.getByLabel(/Bounding box/).fill("-75.6, 39.1, -75.45, 39.2");
  await dlg.getByRole("button", { name: "Add only" }).click();
  await page.getByText(/added but not built yet/).waitFor();
  await shot("03-regions");
});

await step("regions: catalog search", async () => {
  const probe = await fetch(`${base}/admin/api/catalog`, { headers: { cookie: await cookie() } });
  if (probe.status === 503) {
    expected.push("503 GET /admin/api/catalog (catalog not wired in this build)");
    return;
  }
  await page.goto(`${base}/admin/regions?add=1`, { waitUntil: "networkidle" });
  const dlg = page.getByRole("dialog", { name: "Add a region" });
  await dlg.getByLabel("Search regions").fill("dela", { timeout: 20000 });
  await dlg
    .getByRole("button", { name: /^Delaware/ })
    .first()
    .waitFor({ timeout: 20000 });
  await shot("04-catalog");
  await page.keyboard.press("Escape");
});

await step("schedule: change and save", async () => {
  await nav("Schedule");
  await shot("05-schedule");
  const save = page.getByRole("button", { name: /save/i }).first();
  const cron = page.getByLabel(/cron/i).first();
  await cron.fill("15 4 * * 0");
  await save.click();
  await page.waitForLoadState("networkidle");
  const s = await (
    await fetch(`${base}/admin/api/schedule`, { headers: { cookie: await cookie() } })
  ).json();
  if (s.cron !== "15 4 * * 0") throw new Error(`stored cron ${s.cron}`);
});

await step("builds page", async () => {
  await nav("Builds");
  await shot("06-builds");
});

await step("stats page", async () => {
  await nav("Stats");
  await page.waitForTimeout(500);
  await shot("07-stats");
});

await step("system page", async () => {
  await nav("System");
  await page
    .getByText(/OpenStreetMap/)
    .first()
    .waitFor();
  await shot("08-system");
});

await step("map: back to the map with a different style", async () => {
  await nav("Map");
  await page.waitForTimeout(2000);
  await shot("09-map-again");
});

async function cookie() {
  return (await ctx.cookies(base)).map((c) => `${c.name}=${c.value}`).join("; ");
}

await browser.close();
console.log(results.join("\n"));
if (expected.length)
  console.log(`\nexpected failures (allowed):\n  ${[...new Set(expected)].join("\n  ")}`);
if (problems.length) console.log(`\nproblems:\n  ${[...new Set(problems)].join("\n  ")}`);
console.log(`\nscreenshots: ${out}`);
process.exit(results.some((r) => r.startsWith("FAIL")) || problems.length ? 1 : 0);
