// On-demand terrain in the admin UI: its card on the Overview, its section in Settings and
// the per-region choice. The logic lives in lib/ondemand.ts.
import { CloudDownload, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { Link } from "react-router";
import { api, type OnDemandStatus, type Region, type TerrainMode } from "../api";
import * as fmt from "../lib/format";
import {
  appliesTo,
  cacheFill,
  cacheHitRate,
  cacheSizeProblem,
  gbToMb,
  hostState,
  mbToGb,
  sortedHosts,
  TERRAIN_CHOICES,
  terrainMode,
} from "../lib/ondemand";
import {
  Badge,
  Button,
  Card,
  Confirm,
  cx,
  ErrorNote,
  Input,
  Modal,
  Progress,
  Segmented,
} from "./ui";

const label = "text-[11px] font-medium tracking-wide text-faint uppercase";

function Hosts({ s }: { s: OnDemandStatus }) {
  if (s.upstream.length === 0) {
    return <div className="text-xs text-muted">No source has been asked for anything yet.</div>;
  }
  return (
    <ul className="space-y-1.5">
      {sortedHosts(s.upstream).map((h) => {
        const st = hostState(h);
        return (
          <li key={h.host} className="min-w-0 text-xs">
            <Badge tone={st.tone} dot>
              {st.label}
            </Badge>
            <div className="mt-0.5 font-mono text-[11px] break-all text-muted">{h.host}</div>
            {!h.healthy && (
              <div className="mt-0.5 text-bad">
                <span className="line-clamp-2" title={h.error}>
                  {h.error || "The last request failed."}
                </span>
                <span className="text-muted">
                  {h.last_ok
                    ? ` Last answered ${fmt.relTime(h.last_ok)}.`
                    : " It has not answered yet."}
                </span>
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function CacheUse({ s }: { s: OnDemandStatus }) {
  const fill = cacheFill(s);
  return (
    <>
      <div className="tnum mt-0.5 text-sm font-medium" data-testid="ondemand-cache-use">
        {fmt.bytes(s.cache.bytes)} <span className="font-normal text-muted">of</span>{" "}
        {fmt.bytes(s.cache.max_bytes)}
      </div>
      <Progress value={fill} className="mt-1.5 h-1.5" />
      <div className="tnum mt-1 text-xs text-muted">
        {fmt.number(s.cache.entries)} files
        {fill > 0.97 ? " · full: the least recently used are dropped as new ones arrive" : ""}
      </div>
    </>
  );
}

/** The Overview's card: cache use, how much is answered from it, and the DEM hosts' health. */
export function OnDemandCard({ s, names }: { s: OnDemandStatus; names: Record<string, string> }) {
  const rate = cacheHitRate(s.counters);
  return (
    <Card
      title={
        <span className="flex items-center gap-2">
          On-demand terrain
          {!s.enabled && <Badge>off</Badge>}
          {s.enabled && !s.healthy && (
            <Badge tone="bad" dot>
              a source is not answering
            </Badge>
          )}
        </span>
      }
      className="lg:col-span-2"
      action={
        <Link to="/settings" className="text-xs font-medium text-accent-strong hover:underline">
          Settings
        </Link>
      }
    >
      <div className="grid gap-4 sm:grid-cols-3">
        <div className="min-w-0">
          <div className={label}>Cache</div>
          <CacheUse s={s} />
        </div>
        <div className="min-w-0">
          <div className={label}>Since start</div>
          <div className="tnum mt-0.5 text-sm font-medium">
            {rate == null ? "Nothing asked for yet" : `${fmt.percent(rate)} from the cache`}
          </div>
          <div className="tnum text-xs text-muted">
            {fmt.number(s.counters.built)} tiles read from the sources (
            {fmt.bytes(s.counters.bytes)})
          </div>
          {(s.counters.failed > 0 || s.counters.limited > 0) && (
            <div className="tnum mt-0.5 text-xs text-warn">
              {s.counters.failed > 0 && `${fmt.number(s.counters.failed)} could not be read`}
              {s.counters.failed > 0 && s.counters.limited > 0 && " · "}
              {s.counters.limited > 0 &&
                `${fmt.number(s.counters.limited)} held back by rate limits`}
            </div>
          )}
        </div>
        <div className="min-w-0">
          <div className={cx(label, "mb-1")}>Elevation sources</div>
          <Hosts s={s} />
        </div>
      </div>
      <p className="mt-3 border-t border-line pt-2.5 text-xs text-muted">
        {s.enabled ? (
          <>
            Applies to {appliesTo(s, names)}, at zooms {s.minzoom} to {s.maxzoom}. Built terrain is
            always used first.
          </>
        ) : (
          <>Off everywhere. What is cached is kept until it is cleared in Settings.</>
        )}
      </p>
    </Card>
  );
}

/** The Settings page's section: the trade-off, the two settings, cache use and source health. */
export function OnDemandSettings({
  status,
  regions,
  freeBytes,
  onChange,
}: {
  status: OnDemandStatus;
  regions: Region[];
  freeBytes?: number;
  onChange: (s: OnDemandStatus) => void;
}) {
  const [gb, setGb] = useState(String(mbToGb(status.config.cache_mb)));
  const [busy, setBusy] = useState<"outside" | "cache" | "clear" | null>(null);
  const [error, setError] = useState<string>();
  const [clearing, setClearing] = useState(false);
  // Follow the server when it changes under us (another tab), unless the field is being edited.
  const serverGb = String(mbToGb(status.config.cache_mb));
  useEffect(() => setGb(serverGb), [serverGb]);

  const names = Object.fromEntries(regions.map((r) => [r.id, r.name]));
  const typed = Number(gb);
  const problem = cacheSizeProblem(typed);
  const changed = !problem && gbToMb(typed) !== status.config.cache_mb;
  const overFree = freeBytes != null && typed * 1e9 > freeBytes + status.cache.bytes;

  async function run(what: "outside" | "cache" | "clear", fn: () => Promise<OnDemandStatus>) {
    setBusy(what);
    setError(undefined);
    try {
      onChange(await fn());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  return (
    <Card
      title={
        <span className="flex items-center gap-2">
          <CloudDownload className="size-4 text-faint" />
          On-demand terrain
          <Badge tone={status.enabled ? "accent" : "neutral"}>
            {status.enabled ? "on" : "off"}
          </Badge>
        </span>
      }
      action={
        <span className="hidden text-xs text-faint sm:inline">set here, not in the environment</span>
      }
    >
      <div className="space-y-4 text-[13px]">
        <p className="max-w-3xl text-muted">
          Terrain and elevation can be read from the public elevation sources the first time a place
          is asked for, instead of being built ahead. That saves almost all of a region's terrain
          disk and build time. The price: the first view of a place waits about a second while it is
          fetched, this server needs the internet for places it has not seen yet, and what it
          fetches takes cache disk. Built terrain is always used first. Map tiles and routing are
          always built: they cannot be read on demand.
        </p>

        <div className="grid gap-x-6 gap-y-4 border-t border-line pt-4 md:grid-cols-2">
          <div className="min-w-0 space-y-1.5">
            <div className="text-xs font-medium text-muted">Outside built regions</div>
            <div className="flex flex-wrap items-center gap-2">
              <Segmented
                label="Terrain outside built regions"
                value={status.config.outside}
                onChange={(v) =>
                  v !== status.config.outside &&
                  run("outside", () => api.saveOnDemand({ outside: v }))
                }
                options={[
                  { value: "off", label: "Off" },
                  { value: "on_demand", label: "On demand" },
                ]}
              />
              {busy === "outside" && <span className="text-xs text-muted">Saving…</span>}
            </div>
            <p className="text-xs text-faint">
              {status.config.outside === "on_demand"
                ? `Any place on Earth has terrain and elevation from zoom ${status.minzoom} in. Zoomed out further, terrain shows only where a region was built.`
                : "Places outside every region have no terrain or elevation."}
            </p>
          </div>

          <div className="min-w-0 space-y-1.5">
            <div className="text-xs font-medium text-muted">Regions on demand</div>
            <div className="flex flex-wrap gap-1">
              {status.regions.length === 0 ? (
                <span className="text-muted">None: every region's terrain is built.</span>
              ) : (
                status.regions.map((id) => (
                  <Badge key={id} tone="accent">
                    {names[id] ?? id}
                  </Badge>
                ))
              )}
            </div>
            <p className="text-xs text-faint">
              Chosen per region on the{" "}
              <Link to="/regions" className="text-accent-strong hover:underline">
                Regions
              </Link>{" "}
              page. A region on demand still gets its zoomed-out terrain built.
            </p>
          </div>

          <div className="min-w-0 space-y-1.5">
            <label htmlFor="od-cache" className="block text-xs font-medium text-muted">
              Cache size
            </label>
            <div className="flex flex-wrap items-center gap-2">
              <div className="w-24">
                <Input
                  id="od-cache"
                  type="number"
                  inputMode="decimal"
                  min={0.1}
                  step={0.5}
                  value={gb}
                  onChange={(e) => setGb(e.target.value)}
                  aria-describedby="od-cache-hint"
                />
              </div>
              <span className="text-muted">GB</span>
              <Button
                size="sm"
                variant="primary"
                disabled={!changed}
                loading={busy === "cache"}
                onClick={() => run("cache", () => api.saveOnDemand({ cache_mb: gbToMb(typed) }))}
              >
                Save
              </Button>
            </div>
            <p id="od-cache-hint" className={cx("text-xs", problem ? "text-bad" : "text-faint")}>
              {problem ||
                "One budget for fetched tiles and the source data they were made from. When it is full, what was used longest ago is dropped."}
            </p>
            {overFree && !problem && (
              <p className="text-xs text-warn">
                More than the {fmt.bytes(freeBytes)} free on the data volume: the cache would fill
                the disk before it reached this size.
              </p>
            )}
          </div>

          <div className="min-w-0">
            <div className="text-xs font-medium text-muted">In the cache now</div>
            <CacheUse s={status} />
            <Button
              size="sm"
              className="mt-2"
              icon={<Trash2 className="size-3.5" />}
              disabled={status.cache.entries === 0}
              onClick={() => setClearing(true)}
            >
              Clear the cache
            </Button>
          </div>
        </div>

        <div className="border-t border-line pt-4">
          <div className="mb-2 text-xs font-medium text-muted">
            Where it reads from, best source first
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[30rem] text-[13px]">
              <tbody>
                {status.sources.map((src) => {
                  const h = status.upstream.find((u) => u.host === src.host);
                  const st = h ? hostState(h) : { tone: "neutral" as const, label: "Not used yet" };
                  return (
                    <tr key={src.id} className="border-b border-line last:border-0">
                      <td className="py-1.5 pr-3 font-medium">{src.name}</td>
                      <td className="tnum py-1.5 pr-3 whitespace-nowrap text-muted">
                        {src.resolution_m} m
                      </td>
                      <td className="py-1.5 pr-3 font-mono text-[11px] break-all text-muted">
                        {src.host}
                      </td>
                      <td className="py-1.5 text-right">
                        <Badge tone={st.tone} dot>
                          {st.label}
                        </Badge>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p className="mt-2 text-xs text-faint">
            The same public files the terrain build reads. Requests are capped, retried with a
            pause, and identify themselves as mapthing. A source that does not answer is an error
            for that request (HTTP 504) and is never cached.
          </p>
        </div>
        {error && <ErrorNote error={error} />}
      </div>

      <Confirm
        open={clearing}
        title="Clear the on-demand cache?"
        confirmLabel="Clear"
        busy={busy === "clear"}
        onClose={() => setClearing(false)}
        onConfirm={() => run("clear", () => api.clearOnDemand()).then(() => setClearing(false))}
      >
        {fmt.bytes(status.cache.bytes)} is deleted. Places read on demand are fetched again the next
        time they are asked for, which needs the internet. Built terrain is not touched.
      </Confirm>
    </Card>
  );
}

/** Two choices as cards, for a region's terrain. */
export function TerrainChoice({
  value,
  onChange,
  compact,
}: {
  value: TerrainMode;
  onChange: (m: TerrainMode) => void;
  compact?: boolean;
}) {
  return (
    <div role="radiogroup" aria-label="Terrain" className="grid gap-2 sm:grid-cols-2">
      {TERRAIN_CHOICES.map((c) => (
        <button
          key={c.value}
          type="button"
          role="radio"
          aria-checked={value === c.value}
          onClick={() => onChange(c.value)}
          className={cx(
            "flex flex-col justify-start rounded-xl border p-3 text-left transition focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent",
            value === c.value
              ? "border-accent bg-accent-soft"
              : "border-line bg-panel hover:bg-sunken",
          )}
        >
          <div className="flex items-center gap-2 text-sm font-medium">
            <span
              className={cx(
                "inline-block size-3.5 shrink-0 rounded-full border",
                value === c.value
                  ? "border-accent bg-accent ring-2 ring-panel ring-inset"
                  : "border-line-strong",
              )}
            />
            {c.title}
          </div>
          {!compact && <p className="mt-1.5 text-xs text-muted">{c.detail}</p>}
        </button>
      ))}
    </div>
  );
}

/** The dialog that changes one region's terrain between built and on demand. */
export function TerrainModeDialog({
  region,
  onClose,
  onSaved,
}: {
  region: Region | null;
  onClose: () => void;
  onSaved: (r: Region) => void;
}) {
  const [mode, setMode] = useState<TerrainMode>("prebuilt");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const current = region ? terrainMode(region) : "prebuilt";
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset when another region is opened
  useEffect(() => {
    setMode(current);
    setError(undefined);
  }, [region?.id]);

  async function save() {
    if (!region) return;
    setBusy(true);
    setError(undefined);
    try {
      onSaved(await api.setRegionTerrain(region.id, mode));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open={!!region}
      onClose={onClose}
      title={`Terrain for ${region?.name ?? ""}`}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={mode === current} loading={busy} onClick={save}>
            Save
          </Button>
        </>
      }
    >
      <TerrainChoice value={mode} onChange={setMode} />
      <p className="mt-3 text-xs text-muted">
        {mode === current
          ? "Map tiles and routing for the region are built either way."
          : mode === "on_demand"
            ? "From now on, places in this region with no built terrain are read on demand. Terrain already built keeps being served until the next build, which leaves it out and frees its disk."
            : "The next build makes this region's terrain. Until it has, the region keeps being read on demand."}
      </p>
      {error && (
        <div className="mt-3">
          <ErrorNote error={error} />
        </div>
      )}
    </Modal>
  );
}
