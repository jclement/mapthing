// Rate limits in words and numbers, apart from the Keys page's components.
import type { KeyLimits, LimitScope, LimitsInput } from "../api";
/** Limits are exact figures: no "2.0M". */
export const exact = (n: number) => n.toLocaleString("en-US");

export const KIND_NAME: Record<string, string> = {
  tile: "Vector tiles, styles, fonts",
  raster: "Raster tiles",
  route: "Routes",
  matrix: "Matrices",
  isochrone: "Isochrones",
  snap: "Snapping",
  valhalla: "Valhalla pass-through",
  elevation: "Elevation",
  pack: "Offline packs",
  ondemand: "On-demand terrain, read from the sources",
};

export const SCOPE_NAME: Record<LimitScope, string> = {
  rate: "over its rate",
  ip: "one client over the per-client rate",
  daily: "daily quota used up",
  monthly: "monthly quota used up",
  global: "the server's own limit",
};

const perSec = (n: number) => (n >= 1 ? `${+n.toFixed(2)}/s` : `${+(n * 60).toFixed(2)}/min`);

/** One phrase per limit a key has; empty when it has none. */
export function limitsSummary(l: KeyLimits): string[] {
  const out: string[] = [];
  if (l.rps) out.push(`${perSec(l.rps)}${l.burst ? `, burst ${exact(l.burst)}` : ""}`);
  if (l.daily_quota) out.push(`${exact(l.daily_quota)}/day`);
  if (l.monthly_quota) out.push(`${exact(l.monthly_quota)}/month`);
  if (l.ip_rps) out.push(`${perSec(l.ip_rps)} per client`);
  return out;
}

export function hasLimits(l: KeyLimits): boolean {
  return !!(l.rps || l.daily_quota || l.monthly_quota || l.ip_rps);
}

/** Share of a quota used, 0..1 (more than 1 is shown as full). */
export function share(used: number | undefined, quota: number | null | undefined): number {
  if (!quota) return 0;
  return Math.min(1, Math.max(0, (used ?? 0) / quota));
}

export function shareTone(x: number): "accent" | "bad" | "ok" {
  return x >= 1 ? "bad" : "accent";
}

/** What the form holds, as text. */
export interface LimitsForm {
  rps: string;
  burst: string;
  daily: string;
  monthly: string;
  ipRps: string;
  ipBurst: string;
  origins: string;
}

export const EMPTY_FORM: LimitsForm = {
  rps: "",
  burst: "",
  daily: "",
  monthly: "",
  ipRps: "",
  ipBurst: "",
  origins: "",
};

const str = (n: number | null | undefined) => (n ? String(n) : "");

export function formFromLimits(l: KeyLimits): LimitsForm {
  return {
    rps: str(l.rps),
    burst: str(l.burst),
    daily: str(l.daily_quota),
    monthly: str(l.monthly_quota),
    ipRps: str(l.ip_rps),
    ipBurst: str(l.ip_burst),
    origins: l.origins.join(", "),
  };
}

/**
 * The form as the API takes it, or what is wrong with it. An empty field is "no limit"
 * (null), so saving an edit can remove a limit. A burst without its rate is refused: it
 * would do nothing.
 */
export function formToInput(f: LimitsForm): { input: LimitsInput } | { error: string } {
  const num = (v: string, name: string, whole: boolean): number | null | string => {
    const t = v.trim();
    if (t === "") return null;
    const n = Number(t);
    if (!Number.isFinite(n) || n < 0) return `${name} must be a number, or empty for no limit.`;
    if (whole && !Number.isInteger(n)) return `${name} must be a whole number.`;
    return n === 0 ? null : n;
  };
  const fields: [keyof LimitsInput, string, string, boolean][] = [
    ["rps", f.rps, "Rate", false],
    ["burst", f.burst, "Burst", true],
    ["daily_quota", f.daily, "Daily quota", true],
    ["monthly_quota", f.monthly, "Monthly quota", true],
    ["ip_rps", f.ipRps, "Per-client rate", false],
    ["ip_burst", f.ipBurst, "Per-client burst", true],
  ];
  const input: LimitsInput = {};
  for (const [key, value, name, whole] of fields) {
    const n = num(value, name, whole);
    if (typeof n === "string") return { error: n };
    (input as Record<string, number | null>)[key] = n;
  }
  if (input.burst && !input.rps) return { error: "A burst needs a rate." };
  if (input.ip_burst && !input.ip_rps)
    return { error: "A per-client burst needs a per-client rate." };
  if (input.daily_quota && input.monthly_quota && input.daily_quota > input.monthly_quota)
    return { error: "The daily quota is larger than the monthly one, so it would never apply." };
  input.origins = f.origins
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  return { input };
}

/** How many of a kind of request a number of cost units buys, in words. */
export function unitsExample(units: number, weights: Record<string, number> | undefined): string {
  const w = weights ?? { tile: 1, raster: 10, route: 10 };
  const of = (kind: string) => exact(Math.floor(units / (w[kind] || 1)));
  return `${of("tile")} vector tiles, or ${of("raster")} raster tiles, or ${of("route")} routes`;
}
