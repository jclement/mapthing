// End-to-end click-through of the admin UI against a REAL mapthing server (no mocks).
// Usage: MAPTHING_URL=http://127.0.0.1:8080 ADMIN_USER=… ADMIN_PASSWORD=… node scripts/e2e.mjs [outDir]
// With E2E_BUILD=1 (a fresh data dir, network): catalog search → add Delaware and build → watch
// progress over SSE → generation live → route with elevation profile → raster png/jpg/webp.
// Exits non-zero when a step fails or the page logs errors / gets 5xx responses.
import { mkdirSync } from "node:fs";
import { chromium } from "playwright";

const BUILD = process.env.E2E_BUILD === "1";
const BUILD_REGION = process.env.E2E_REGION ?? "Delaware";
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
let live = !BUILD; // a generation is live (BUILD mode starts from an empty data dir)
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
  if (r.status() === 503 && u.includes("/tiles/") && !live) return; // nothing built yet
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
  await page.getByRole("link", { name: "Map", exact: true }).first().waitFor({ timeout: 15000 });
  await page.waitForTimeout(2500); // tiles, glyphs, sprites
  await shot("01-map");
});

if (!BUILD)
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
  const r = await fetch(`${base}/styles?key=${created}`);
  if (r.status !== 200) throw new Error(`status ${r.status}`); // /styles answers before any build
});

await step("keys: revoke", async () => {
  await page.getByRole("button", { name: `Revoke ${label}` }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Revoke key" }).click();
  await page.getByRole("button", { name: `Revoke ${label}` }).waitFor({ state: "detached" });
  const r = await fetch(`${base}/styles?key=${created}`);
  if (r.status !== 401) throw new Error(`revoked key answered ${r.status}`);
});

if (BUILD) await buildFlow();

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

if (!BUILD)
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

async function api(method, path, body) {
  const r = await fetch(`${base}${path}`, {
    method,
    headers: { cookie: await cookie(), origin: base, "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  return r;
}

async function buildFlow() {
  let key = "";
  await step(`build: catalog search → add ${BUILD_REGION} → build`, async () => {
    await page.goto(`${base}/admin/regions?add=1`, { waitUntil: "networkidle" });
    // count SSE events in this page (it stays loaded: the UI navigates client-side from here)
    await page.evaluate(() => {
      window.__ev = { job: 0, log: 0, generation: 0 };
      const es = new EventSource("/admin/api/events");
      for (const t of ["job", "log", "generation"]) es.addEventListener(t, () => window.__ev[t]++);
    });
    const dlg = page.getByRole("dialog", { name: "Add a region" });
    await dlg
      .getByLabel("Search regions")
      .fill(BUILD_REGION.slice(0, 4).toLowerCase(), { timeout: 60000 });
    await dlg
      .getByRole("button", { name: new RegExp(`^${BUILD_REGION}`) })
      .first()
      .click();
    await dlg.getByTestId("estimate").waitFor();
    await shot("10-catalog-estimate");
    await dlg.getByRole("button", { name: "Add and build" }).click();
    await page.getByRole("heading", { name: "Builds" }).first().waitFor();
    await page.getByText("Current build").waitFor({ timeout: 30000 });
    await shot("11-build-running");
  });

  const t0 = Date.now();
  let job;
  await step("build: progress until the generation is live (SSE)", async () => {
    const deadline = Date.now() + Number(process.env.E2E_BUILD_TIMEOUT_S ?? 2400) * 1000;
    let lastStage = "";
    for (;;) {
      const jobs = (await (await api("GET", "/admin/api/jobs")).json()).jobs;
      job = jobs[0];
      const running = job?.stages?.find((s) => s.status === "running")?.name ?? "";
      if (running && running !== lastStage) {
        console.log(
          `  ${Math.round((Date.now() - t0) / 1000)}s stage ${running} (${Math.round(job.progress * 100)}%)`,
        );
        lastStage = running;
        await shot(`12-build-${running}`);
      }
      if (
        job &&
        (job.status === "succeeded" || job.status === "failed" || job.status === "cancelled")
      )
        break;
      if (Date.now() > deadline) throw new Error(`timed out in stage ${running}`);
      if (!job && Date.now() - t0 > 60000) throw new Error("no build job was queued");
      await page.waitForTimeout(5000);
    }
    if (job.status !== "succeeded") {
      const detail = await (await api("GET", `/admin/api/jobs/${job.id}`)).json();
      console.log(
        detail.log
          .slice(-30)
          .map((l) => `    ${l.msg}`)
          .join("\n"),
      );
      throw new Error(`build ${job.status}: ${job.error}`);
    }
    const ev = (await page.evaluate(() => window.__ev)) ?? { job: 0, log: 0 };
    console.log(`  build took ${Math.round((Date.now() - t0) / 1000)}s; SSE events`, ev);
    if (ev.job < 5 || ev.log < 5) throw new Error(`too few SSE events ${JSON.stringify(ev)}`);
    if (!job.generation_id) throw new Error("job has no generation_id");
    const gens = (await (await api("GET", "/admin/api/generations")).json()).generations;
    if (gens[0]?.status !== "live" || gens[0].id !== job.generation_id)
      throw new Error(`live generation ${gens[0]?.id} != ${job.generation_id}`);
    live = true;
    await page.getByText(/Idle/).waitFor({ timeout: 30000 });
    await shot("13-build-done");
  });

  await step("build: region installed, system ready", async () => {
    const regions = (await (await api("GET", "/admin/api/regions")).json()).regions;
    if (!regions.some((r) => r.status === "installed")) throw new Error(JSON.stringify(regions));
    for (let i = 0; i < 24; i++) {
      const sys = await (await api("GET", "/admin/api/system")).json();
      if (sys.ready) return;
      await page.waitForTimeout(5000);
    }
    const sys = await (await api("GET", "/admin/api/system")).json();
    throw new Error(`not ready: ${JSON.stringify(sys.readiness)}`);
  });

  await step("map: style and basemap tiles", async () => {
    await page.goto(`${base}/admin/map`, { waitUntil: "networkidle" });
    await page.locator(".maplibregl-canvas").waitFor({ timeout: 15000 });
    await page.waitForTimeout(3000);
    await shot("14-map-live");
    if (!seen.style || !seen.tiles)
      throw new Error(`style ${seen.style}, basemap tiles ${seen.tiles}`);
  });

  await step("map: route with elevation profile", async () => {
    await page.getByRole("radio", { name: /Route/ }).click();
    const box = await page.locator(".maplibregl-canvas").boundingBox();
    const routed = page.waitForResponse((r) => r.url().endsWith("/api/v1/route"), {
      timeout: 30000,
    });
    for (const [fx, fy] of [
      [0.47, 0.45],
      [0.53, 0.58],
    ]) {
      await page.mouse.click(box.x + box.width * fx, box.y + box.height * fy);
      await page.waitForTimeout(800);
    }
    const r = await routed;
    if (r.status() !== 200) throw new Error(`route ${r.status()}: ${await r.text()}`);
    await page.getByRole("img", { name: /Elevation profile/ }).waitFor({ timeout: 30000 });
    await page.waitForTimeout(1500);
    await shot("15-route-profile");
  });

  await step("raster: png, jpg, webp through the real renderer", async () => {
    const created = await (await api("POST", "/admin/api/keys", { label: "e2e raster" })).json();
    key = created.secret;
    const regions = (await (await api("GET", "/admin/api/regions")).json()).regions;
    const b = regions.find((r) => r.status === "installed").bbox;
    const [lon, lat] = [(b[0] + b[2]) / 2, (b[1] + b[3]) / 2];
    const z = 12;
    const n = 2 ** z;
    const x = Math.floor(((lon + 180) / 360) * n);
    const rad = (lat * Math.PI) / 180;
    const y = Math.floor(((1 - Math.log(Math.tan(rad) + 1 / Math.cos(rad)) / Math.PI) / 2) * n);
    const magic = {
      "image/png": [0x89, 0x50],
      "image/jpeg": [0xff, 0xd8],
      "image/webp": [0x52, 0x49],
    };
    const cases = [
      [`/styles/outdoors/512/${z}/${x}/${y}@2x.png`, "image/png"],
      [`/styles/outdoors/256/${z}/${x}/${y}.jpg`, "image/jpeg"],
      [`/styles/outdoors-dark/512/${z}/${x}/${y}.webp`, "image/webp"],
      [`/styles/v1/u/satellite/tiles/512/${z}/${x}/${y}@2x`, "image/jpeg"],
      [`/styles/v1/u/outdoors/tiles/256/${z}/${x}/${y}`, "image/png"],
    ];
    for (const [path, ct] of cases) {
      const t = Date.now();
      const r = await fetch(`${base}${path}?key=${key}`);
      const body = Buffer.from(await r.arrayBuffer());
      const ok =
        r.status === 200 &&
        r.headers.get("content-type") === ct &&
        body[0] === magic[ct][0] &&
        body[1] === magic[ct][1];
      console.log(
        `  ${path} → ${r.status} ${r.headers.get("content-type")} ${body.length} B ${Date.now() - t} ms${r.headers.get("x-mapthing-degraded") ? " (degraded)" : ""}`,
      );
      if (!ok)
        throw new Error(
          `${path}: ${r.status} ${r.headers.get("content-type")} ${body.subarray(0, 120).toString()}`,
        );
    }
  });
  if (process.env.E2E_SWAP !== "0" && key) await swapFlow(key);
}

async function swapFlow(key) {
  await step("swap: rebuild while routing — zero downtime", async () => {
    const regions = (await (await api("GET", "/admin/api/regions")).json()).regions;
    const b = regions.find((r) => r.status === "installed").bbox;
    const [lon, lat] = [(b[0] + b[2]) / 2, (b[1] + b[3]) / 2];
    const routeBody = JSON.stringify({
      locations: [
        { lat, lon },
        { lat: lat + 0.03, lon: lon + 0.03 },
      ],
      profile: "auto",
    });
    const before = (await (await api("GET", "/admin/api/generations")).json()).generations[0].id;
    const started = await api("POST", "/admin/api/build", { refresh_downloads: false });
    if (started.status !== 202) throw new Error(`build ${started.status}`);
    const stats = { route: 0, routeFail: 0, ready: 0, readyFail: 0, fails: [] };
    let done = false;
    const hammer = (async () => {
      while (!done) {
        const [r1, r2] = await Promise.all([
          fetch(`${base}/api/v1/route?key=${key}`, { method: "POST", body: routeBody }).catch(
            (e) => ({ status: 0, e }),
          ),
          fetch(`${base}/readyz`).catch((e) => ({ status: 0, e })),
        ]);
        stats.route++;
        stats.ready++;
        if (r1.status !== 200) {
          stats.routeFail++;
          stats.fails.push(`route ${r1.status} ${new Date().toISOString()}`);
        }
        if (r2.status !== 200) {
          stats.readyFail++;
          stats.fails.push(`readyz ${r2.status} ${new Date().toISOString()}`);
        }
        await new Promise((res) => setTimeout(res, 250));
      }
    })();
    const deadline = Date.now() + 2400 * 1000;
    let job;
    for (;;) {
      job = (await (await api("GET", "/admin/api/jobs")).json()).jobs[0];
      if (["succeeded", "failed", "cancelled"].includes(job.status)) break;
      if (Date.now() > deadline) break;
      await new Promise((res) => setTimeout(res, 5000));
    }
    // keep routing a little after the swap: the old instance stops after the switch
    await new Promise((res) => setTimeout(res, 10000));
    done = true;
    await hammer;
    const after = (await (await api("GET", "/admin/api/generations")).json()).generations[0].id;
    console.log(
      `  swap ${before} → ${after}: ${stats.route} routes (${stats.routeFail} failed), ${stats.ready} readyz (${stats.readyFail} failed)`,
    );
    if (stats.fails.length) console.log(`    ${stats.fails.slice(0, 10).join("\n    ")}`);
    if (job.status !== "succeeded") throw new Error(`second build ${job.status}: ${job.error}`);
    if (after === before) throw new Error("generation did not change");
    if (stats.routeFail || stats.readyFail) throw new Error("requests failed during the swap");
  });
}

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
