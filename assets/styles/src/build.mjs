// Builds every theme in src/themes/*.mjs (files starting with "_" are shared helpers) into
// assets/styles/<name>.json and validates each against the mapbox-gl 1.13 and MapLibre specs.
//
//   node src/build.mjs            write the styles (fails on validation errors, writes nothing)
//   node src/build.mjs --check    fail if any committed style differs from what the generator makes
//   node src/build.mjs outdoors   build only the named theme(s)
import { readdirSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { buildStyle } from "./layers.mjs";
import { validateStyle } from "./validate.mjs";

const here = new URL(".", import.meta.url);
const outDir = new URL("../", import.meta.url);
const args = process.argv.slice(2);
const check = args.includes("--check");
const only = args.filter((a) => !a.startsWith("--"));

const files = readdirSync(new URL("themes/", here))
  .filter((f) => f.endsWith(".mjs") && !f.startsWith("_"))
  .sort();

const results = [];
let problems = [];
for (const f of files) {
  const theme = (await import(new URL(`themes/${f}`, here))).default;
  if (!theme?.name) throw new Error(`themes/${f}: default export needs a name`);
  if (`${theme.name}.mjs` !== f) throw new Error(`themes/${f}: name "${theme.name}" must match the file name`);
  if (only.length && !only.includes(theme.name)) continue;
  const style = buildStyle(theme);
  const ids = new Set();
  for (const l of style.layers) {
    if (ids.has(l.id)) problems.push(`${theme.name}: duplicate layer id ${l.id}`);
    ids.add(l.id);
  }
  const text = JSON.stringify(style, null, 1) + "\n";
  problems.push(...validateStyle(theme.name, text));
  const vis = style.layers.find((l) => l.id === "satellite")?.layout?.visibility ?? "visible";
  if (vis !== (theme.satellite.visible ? "visible" : "none")) problems.push(`${theme.name}: satellite visibility ${vis}`);
  results.push({ name: theme.name, text, layers: style.layers.length });
}

if (only.length && results.length !== only.length) problems.push(`unknown theme in ${only.join(", ")}`);
if (problems.length) {
  for (const p of problems) console.error(p);
  console.error(`${problems.length} problem(s); nothing written`);
  process.exit(1);
}

let stale = 0;
for (const r of results) {
  const out = new URL(`${r.name}.json`, outDir);
  if (check) {
    const cur = existsSync(out) ? readFileSync(out, "utf8") : "";
    if (cur !== r.text) {
      stale++;
      console.error(`assets/styles/${r.name}.json is out of date: run \`mise run styles\``);
    } else console.log(`${r.name}.json: up to date, valid (${r.layers} layers)`);
  } else {
    writeFileSync(out, r.text);
    console.log(`${r.name}.json: written, valid (${r.layers} layers)`);
  }
}
if (check) {
  // a committed style with no theme is stale too
  for (const f of readdirSync(outDir).filter((f) => f.endsWith(".json") && !f.startsWith("package"))) {
    if (!only.length && !results.some((r) => `${r.name}.json` === f)) {
      stale++;
      console.error(`assets/styles/${f} has no theme in src/themes/`);
    }
  }
}
process.exit(stale ? 1 : 0);
