import {
  AlertTriangle,
  ArrowRight,
  CheckCircle2,
  CircleDashed,
  Hammer,
  Layers,
  ScrollText,
  XCircle,
} from "lucide-react";
import type { Map as MlMap } from "maplibre-gl";
import { useCallback, useMemo } from "react";
import { Link } from "react-router";
import { api, defaultStyle, type Overview, type ProcessInfo, styleUrl } from "../api";
import { Sparkline } from "../components/charts";
import { blankStyle, MapCanvas } from "../components/MapCanvas";
import {
  Badge,
  Card,
  cx,
  ErrorNote,
  Page,
  PageHeader,
  Progress,
  Spinner,
  Stat,
} from "../components/ui";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import { bboxPolygon } from "../lib/geo";
import {
  diskShares,
  headline,
  KIND_LABEL,
  latency,
  logLink,
  needsAttention,
  processLabel,
  processTone,
  rate,
  totals,
  unionBBox,
} from "../lib/overview";
import { useTheme } from "../lib/theme";
import { useApi } from "../lib/useApi";
import { stageLabel } from "./Builds";

export function OverviewPage() {
  const { generationTick, active } = useEvents();
  const s = useApi(() => api.overview(), [generationTick, active?.status], 5000);
  const o = s.data;
  if (!o) {
    return (
      <Page wide>
        <PageHeader title="Overview" />
        {s.error ? <ErrorNote error={s.error} onRetry={s.reload} /> : <Spinner />}
      </Page>
    );
  }
  const h = headline(o);
  const HeadIcon =
    h.tone === "ok"
      ? CheckCircle2
      : h.tone === "bad"
        ? XCircle
        : h.tone === "info"
          ? CircleDashed
          : AlertTriangle;
  return (
    <Page wide>
      <PageHeader
        title="Overview"
        description={
          <>
            mapthing {o.version}
            {o.commit ? ` (${o.commit})` : ""} · up {fmt.duration(secondsSince(o.started_at))}
          </>
        }
        actions={
          s.error ? (
            <Badge tone="warn" dot>
              Not updating: {s.error.message}
            </Badge>
          ) : undefined
        }
      />

      <div
        role="status"
        className={cx(
          "mb-5 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-(--radius-card) border px-4 py-3",
          h.tone === "ok" && "border-ok/30 bg-ok/8",
          h.tone === "bad" && "border-bad/30 bg-bad/8",
          h.tone === "warn" && "border-warn/40 bg-warn/10",
          h.tone === "info" && "border-info/30 bg-info/8",
        )}
      >
        <HeadIcon
          className={cx(
            "size-5 shrink-0",
            h.tone === "ok" && "text-ok",
            h.tone === "bad" && "text-bad",
            h.tone === "warn" && "text-warn",
            h.tone === "info" && "text-info",
          )}
        />
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold">{h.title}</div>
          <div className="text-[13px] text-muted">{h.detail}</div>
        </div>
        {!o.generation && !o.builds.active && (
          <Link
            to={o.regions_installed === 0 ? "/regions?add=1" : "/builds"}
            className="inline-flex h-9 items-center gap-1.5 rounded-lg bg-accent px-3.5 text-sm font-medium text-accent-fg shadow-sm hover:bg-accent-strong"
          >
            {o.regions_installed === 0 ? (
              <>
                <Layers className="size-4" /> Add a region
              </>
            ) : (
              <>
                <Hammer className="size-4" /> Go to Builds
              </>
            )}
          </Link>
        )}
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3 [&>*]:min-w-0">
        <LiveCard o={o} />
        <ProcessesCard o={o} />
        <TrafficCard o={o} />
        <ResourcesCard o={o} />
        <BuildsCard o={o} />
      </div>
    </Page>
  );
}

function secondsSince(iso: string | undefined) {
  return iso ? Math.max(0, (Date.now() - Date.parse(iso)) / 1000) : 0;
}

// ---------- what is live ----------

function LiveCard({ o }: { o: Overview }) {
  const { dark } = useTheme();
  const g = o.generation;
  const styles = useApi(() => api.styles(), []);
  const bounds = useMemo(() => (g ? unionBBox(g.regions) : undefined), [g]);
  const regionsKey = g?.regions.map((r) => `${r.id}:${r.bbox?.join(",")}`).join("|") ?? "";
  // biome-ignore lint/correctness/useExhaustiveDependencies: regionsKey stands for g.regions
  const draw = useCallback(
    (map: MlMap) => {
      if (!g) return;
      const data = {
        type: "FeatureCollection" as const,
        features: g.regions
          .filter((r) => r.bbox?.length === 4)
          .map((r) => ({
            type: "Feature" as const,
            properties: { name: r.name },
            geometry: { type: "Polygon" as const, coordinates: [bboxPolygon(r.bbox!)] },
          })),
      };
      if (map.getSource("live-regions")) return;
      map.addSource("live-regions", { type: "geojson", data });
      map.addLayer({
        id: "live-regions-fill",
        type: "fill",
        source: "live-regions",
        paint: { "fill-color": "#14b8a6", "fill-opacity": 0.12 },
      });
      map.addLayer({
        id: "live-regions-line",
        type: "line",
        source: "live-regions",
        paint: { "line-color": "#14b8a6", "line-width": 2 },
      });
    },
    [regionsKey],
  );
  return (
    <Card
      title="Live map"
      className="lg:col-span-2"
      action={
        g && (
          <Link to="/map" className="text-xs font-medium text-accent-strong hover:underline">
            Open the map
          </Link>
        )
      }
      pad={false}
    >
      {g ? (
        <div className="grid sm:grid-cols-[minmax(0,1fr)_minmax(0,17rem)]">
          <div className="relative h-56 overflow-hidden sm:h-auto sm:min-h-64 sm:rounded-bl-(--radius-card)">
            <MapCanvas
              key={`${g.id}|${regionsKey}`}
              style={
                styles.data || styles.error
                  ? styleUrl(defaultStyle(styles.data, dark))
                  : blankStyle(dark)
              }
              initialBounds={bounds}
              onStyle={draw}
            />
          </div>
          <div className="border-t border-line p-4 sm:border-t-0 sm:border-l">
            <div className="text-[11px] font-medium tracking-wide text-faint uppercase">
              Generation
            </div>
            <Link
              to="/generations"
              className="font-mono text-[13px] font-medium hover:text-accent-strong"
            >
              {g.id}
            </Link>
            <div className="text-xs text-muted">
              {g.created_at ? `built ${fmt.relTime(g.created_at)} · ` : ""}
              {fmt.bytes(g.bytes)}
            </div>
            <div className="mt-3 text-[11px] font-medium tracking-wide text-faint uppercase">
              {g.regions.length} region{g.regions.length === 1 ? "" : "s"}
            </div>
            <ul className="mt-1 max-h-44 space-y-1 overflow-y-auto text-[13px]">
              {g.regions.map((r) => (
                <li key={r.id} className="flex items-baseline justify-between gap-2">
                  <span className="truncate">{r.name || r.id}</span>
                  <span className="shrink-0 text-xs text-faint" title="OpenStreetMap data date">
                    {r.data_date ? fmt.date(r.data_date) : ""}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        </div>
      ) : (
        <div className="px-6 py-10 text-center">
          <h3 className="text-sm font-semibold">No map is live</h3>
          <p className="mx-auto mt-1 max-w-md text-sm text-muted">
            mapthing serves what it builds. Choose regions (a country, a state or province, or a box
            you draw), then build: the first build downloads OpenStreetMap and elevation data and
            makes the tiles, routing graph and terrain.
          </p>
          <ol className="mx-auto mt-4 flex max-w-md flex-col gap-2 text-left text-[13px]">
            <Step n={1} done={o.regions_installed > 0} to="/regions?add=1">
              Add a region
            </Step>
            <Step n={2} done={false} to="/builds">
              Build it{o.builds.active ? " (running now)" : ""}
            </Step>
            <Step n={3} done={false} to="/keys">
              Create an API key and point your app at it
            </Step>
          </ol>
        </div>
      )}
    </Card>
  );
}

function Step({
  n,
  done,
  to,
  children,
}: {
  n: number;
  done: boolean;
  to: string;
  children: React.ReactNode;
}) {
  return (
    <li>
      <Link
        to={to}
        className="flex items-center gap-2.5 rounded-lg border border-line px-3 py-2 hover:bg-sunken"
      >
        <span
          className={cx(
            "grid size-5 shrink-0 place-items-center rounded-full text-[11px] font-semibold",
            done ? "bg-ok text-white" : "bg-accent-soft text-accent-strong",
          )}
        >
          {done ? "✓" : n}
        </span>
        <span className={cx("flex-1", done && "text-muted line-through")}>{children}</span>
        <ArrowRight className="size-3.5 text-faint" />
      </Link>
    </li>
  );
}

// ---------- processes ----------

function ProcessesCard({ o }: { o: Overview }) {
  const rss = o.processes.reduce((a, p) => a + p.rss_bytes, 0);
  return (
    <Card
      title="Processes"
      action={<span className="tnum text-xs text-faint">{fmt.bytes(rss)} in use</span>}
    >
      <ul className="space-y-3">
        {o.processes.map((p) => (
          <ProcessRow key={p.name} p={p} />
        ))}
      </ul>
      <p className="mt-3 border-t border-line pt-3 text-xs text-faint">
        Memory includes what each process started. CPU is a share of one core ({o.system.cpus}{" "}
        available).
      </p>
    </Card>
  );
}

function ProcessRow({ p }: { p: ProcessInfo }) {
  const attention = needsAttention(p);
  return (
    <li>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
            <span className="font-mono text-[13px]">{p.name}</span>
            <Badge tone={processTone(p)} dot>
              {processLabel(p)}
            </Badge>
          </div>
          <div className="truncate text-xs text-faint">
            {p.pid ? `up ${fmt.duration(p.uptime_s)}` : (p.detail ?? "not running")}
            {p.name !== "mapthing" && p.name !== "build tools" && (
              <>
                {" · "}
                <span className={cx(p.restarts > 0 && "text-warn")}>
                  {p.restarts} restart{p.restarts === 1 ? "" : "s"}
                </span>
              </>
            )}
            {p.role ? ` · ${p.role}` : ""}
          </div>
          {attention && p.detail && <div className="text-xs text-bad">{p.detail}</div>}
        </div>
        <div className="shrink-0 text-right">
          <div className="tnum text-[13px] font-medium">
            {p.rss_bytes ? fmt.bytes(p.rss_bytes) : "—"}
          </div>
          <div className="tnum text-xs text-faint">
            {p.cpu_percent == null
              ? "CPU —"
              : `CPU ${p.cpu_percent.toFixed(p.cpu_percent < 10 ? 1 : 0)}%`}
          </div>
        </div>
      </div>
      {(attention || p.restarts > 0) && p.name !== "mapthing" && (
        <Link
          to={logLink(p)}
          className="mt-1 inline-flex items-center gap-1 text-xs font-medium text-accent-strong hover:underline"
        >
          <ScrollText className="size-3.5" />
          {attention ? "See why in its log" : "Its log, with the restarts"}
        </Link>
      )}
    </li>
  );
}

// ---------- traffic ----------

function TrafficCard({ o }: { o: Overview }) {
  const kinds = o.traffic.kinds;
  const t = totals(kinds);
  return (
    <Card
      title="Traffic"
      className="lg:col-span-2"
      action={
        <Link to="/stats" className="text-xs font-medium text-accent-strong hover:underline">
          Usage by day and key
        </Link>
      }
    >
      <div className="mb-4 grid grid-cols-2 gap-4 sm:grid-cols-4">
        <Stat label="Now" value={rate(t.perMin)} sub="mean of the last hour" />
        <Stat label="Last hour" value={fmt.number(t.hour)} sub={errorsNote(t.errorsHour)} />
        <Stat label="Last 24 hours" value={fmt.number(t.day)} sub={errorsNote(t.errorsDay)} />
        <div className="min-w-0">
          <div className="text-[11px] font-medium tracking-wide text-faint uppercase">
            Per minute, last hour
          </div>
          <Sparkline values={t.series} width={150} height={34} className="mt-1" />
        </div>
      </div>
      {t.day === 0 ? (
        <p className="rounded-lg bg-sunken px-3 py-4 text-center text-sm text-muted">
          No requests since the server started {fmt.relTime(o.traffic.since)}. Rates and latency
          appear here as soon as a client asks for a tile, a route or an elevation.
        </p>
      ) : (
        <div className="-mx-4 overflow-x-auto px-4">
          <table className="w-full min-w-[34rem] text-[13px]">
            <thead>
              <tr className="text-left text-[11px] tracking-wide text-faint uppercase">
                <th className="pb-1.5 font-medium">Kind</th>
                <th className="pb-1.5 text-right font-medium" colSpan={3}>
                  Last hour
                </th>
                <th className="pb-1.5 text-right font-medium" colSpan={3}>
                  Last 24 hours
                </th>
              </tr>
              <tr className="border-b border-line text-right text-[11px] text-faint">
                <th />
                <th className="pb-1.5 font-normal">rate</th>
                <th className="pb-1.5 font-normal">p50</th>
                <th className="pb-1.5 font-normal">p95</th>
                <th className="pb-1.5 font-normal">requests</th>
                <th className="pb-1.5 font-normal">p50</th>
                <th className="pb-1.5 font-normal">p95</th>
              </tr>
            </thead>
            <tbody>
              {kinds.map((k) => (
                <tr key={k.kind} className="border-b border-line last:border-0">
                  <td className="py-1.5">
                    {KIND_LABEL[k.kind] ?? k.kind}
                    {k.day.errors > 0 && (
                      <span className="ml-2 text-xs text-bad">
                        {fmt.number(k.day.errors)} failed
                      </span>
                    )}
                    {k.day.rejected > 0 && (
                      <span
                        className="ml-2 text-xs text-faint"
                        title="Answered 401, 403 or 429: no key, a wrong key, or over a limit"
                      >
                        {fmt.number(k.day.rejected)} refused
                      </span>
                    )}
                  </td>
                  <td className="tnum py-1.5 text-right">{rate(k.hour.per_min)}</td>
                  <td className="tnum py-1.5 text-right text-muted">{latency(k.hour.p50_ms)}</td>
                  <td className="tnum py-1.5 text-right text-muted">{latency(k.hour.p95_ms)}</td>
                  <td className="tnum py-1.5 text-right">{fmt.number(k.day.requests)}</td>
                  <td className="tnum py-1.5 text-right text-muted">{latency(k.day.p50_ms)}</td>
                  <td className="tnum py-1.5 text-right text-muted">{latency(k.day.p95_ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="mt-3 text-xs text-faint">
        Measured in this server since it started ({fmt.relTime(o.traffic.since)}); a restart clears
        it. Time is from request to last byte, so it includes a slow client.
      </p>
    </Card>
  );
}

function errorsNote(n: number) {
  return n > 0 ? <span className="text-bad">{fmt.number(n)} failed (5xx)</span> : "none failed";
}

// ---------- resources ----------

function ResourcesCard({ o }: { o: Overview }) {
  const d = o.disk;
  const sh = diskShares(d);
  const mem = o.system.memory_total_bytes
    ? o.system.memory_used_bytes / o.system.memory_total_bytes
    : 0;
  const g = d.guard;
  return (
    <Card title="Resources" className="lg:row-span-2">
      <div className="space-y-4">
        <div>
          <div className="mb-1 flex items-baseline justify-between text-[13px]">
            <span>Memory</span>
            <span className="tnum text-muted">
              {o.system.memory_total_bytes
                ? `${fmt.bytes(o.system.memory_used_bytes)} of ${fmt.bytes(o.system.memory_total_bytes)}`
                : "not reported on this system"}
            </span>
          </div>
          <Progress value={mem} tone={mem > 0.92 ? "bad" : "accent"} />
          <div className="mt-1 text-xs text-faint">
            Whole machine. {o.system.cpus} CPU{o.system.cpus === 1 ? "" : "s"}
            {o.system.load1 >= 0 ? `, load ${o.system.load1.toFixed(2)}` : ""}.
          </div>
        </div>

        <div>
          <div className="mb-1 flex items-baseline justify-between text-[13px]">
            <span>Disk</span>
            <span className="tnum text-muted">
              {fmt.bytes(d.free_bytes)} free of {fmt.bytes(d.total_bytes)}
            </span>
          </div>
          <div
            className="relative flex h-2.5 overflow-hidden rounded-full bg-sunken ring-1 ring-line ring-inset"
            role="img"
            aria-label={`Disk: mapthing ${fmt.bytes(d.data_bytes)}, other ${fmt.bytes(sh.otherBytes)}, free ${fmt.bytes(d.free_bytes)}`}
          >
            <div className="bg-accent" style={{ width: `${sh.data * 100}%` }} />
            <div className="bg-line-strong" style={{ width: `${sh.other * 100}%` }} />
            {sh.need != null && (
              // Free space is the right-hand end; a build needs it to reach back to this mark.
              <div
                className={cx("absolute inset-y-0 w-0.5", g.ok ? "bg-ok" : "bg-bad")}
                style={{ right: `${sh.need * 100}%` }}
              />
            )}
          </div>
          <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted">
            <span>
              <span className="mr-1 inline-block size-2 rounded-sm bg-accent" />
              mapthing {fmt.bytes(d.data_bytes)}
            </span>
            <span>
              <span className="mr-1 inline-block size-2 rounded-sm bg-line-strong" />
              other {fmt.bytes(sh.otherBytes)}
            </span>
          </div>
          <div
            className={cx(
              "mt-2 rounded-lg px-2.5 py-1.5 text-xs",
              !g.known ? "bg-sunken text-muted" : g.ok ? "bg-ok/8 text-ok" : "bg-bad/8 text-bad",
            )}
          >
            {!g.known
              ? o.regions_installed === 0
                ? "The build disk check runs once regions are chosen."
                : "Working out how much disk the next build needs…"
              : g.ok
                ? `The next build needs about ${fmt.bytes(g.need_bytes)} free: it fits.`
                : `The next build needs about ${fmt.bytes(g.need_bytes)} free and will refuse to start. Free ${fmt.bytes((g.need_bytes ?? 0) - d.free_bytes)} more, or remove a region.`}
          </div>
        </div>

        <div>
          <div className="mb-1.5 text-[11px] font-medium tracking-wide text-faint uppercase">
            Data folder
          </div>
          <dl className="space-y-1 text-[13px]">
            {d.parts
              .filter((p) => p.bytes > 0)
              .map((p) => (
                <div key={p.name} className="flex justify-between gap-3">
                  <dt className="truncate">{p.name}</dt>
                  <dd className="tnum text-muted">{fmt.bytes(p.bytes)}</dd>
                </div>
              ))}
            {d.parts.every((p) => p.bytes === 0) && <div className="text-muted">Empty so far.</div>}
          </dl>
        </div>
      </div>
    </Card>
  );
}

// ---------- builds ----------

function BuildsCard({ o }: { o: Overview }) {
  const b = o.builds;
  const running = b.active?.stages.find((s) => s.status === "running");
  return (
    <Card
      title="Builds"
      className="lg:col-span-2"
      action={
        <Link to="/builds" className="text-xs font-medium text-accent-strong hover:underline">
          All builds
        </Link>
      }
    >
      <div className="grid gap-4 sm:grid-cols-3">
        <div className="min-w-0">
          <div className="text-[11px] font-medium tracking-wide text-faint uppercase">Now</div>
          {b.active ? (
            <>
              <div className="mt-0.5 text-sm font-medium">
                {b.active.status === "queued"
                  ? "Queued"
                  : running
                    ? stageLabel(running.name)
                    : "Finishing"}
              </div>
              <Progress value={b.active.progress} live className="mt-1.5 h-1.5" />
              <div className="tnum mt-1 text-xs text-muted">
                {fmt.percent(b.active.progress)}
                {b.active.started_at ? ` · started ${fmt.relTime(b.active.started_at)}` : ""}
              </div>
            </>
          ) : (
            <>
              <div className="mt-0.5 text-sm font-medium">Idle</div>
              <div className="text-xs text-muted">No build is running.</div>
            </>
          )}
        </div>
        <div className="min-w-0">
          <div className="text-[11px] font-medium tracking-wide text-faint uppercase">
            Next scheduled
          </div>
          {b.schedule_enabled && b.next_run ? (
            <>
              <div className="mt-0.5 text-sm font-medium">{fmt.relTime(b.next_run)}</div>
              <div className="text-xs text-muted">{fmt.dateTime(b.next_run)}</div>
            </>
          ) : (
            <>
              <div className="mt-0.5 text-sm font-medium">Not scheduled</div>
              <div className="text-xs text-muted">
                Data is refreshed only when you build.{" "}
                <Link to="/schedule" className="text-accent-strong hover:underline">
                  Set a schedule
                </Link>
              </div>
            </>
          )}
        </div>
        <div className="min-w-0">
          <div className="text-[11px] font-medium tracking-wide text-faint uppercase">
            Last build
          </div>
          {b.last ? (
            <>
              <div className="mt-0.5 flex flex-wrap items-center gap-2 text-sm font-medium">
                <Badge
                  tone={
                    b.last.status === "succeeded"
                      ? "ok"
                      : b.last.status === "failed"
                        ? "bad"
                        : "neutral"
                  }
                  dot
                >
                  {b.last.status}
                </Badge>
                {b.last.duration_s != null && (
                  <span className="tnum">in {fmt.duration(b.last.duration_s)}</span>
                )}
              </div>
              <div className="text-xs text-muted">
                {fmt.relTime(b.last.finished_at)} ·{" "}
                {b.last.trigger === "schedule" ? "scheduled" : "started by hand"}
              </div>
              {b.last.error && (
                <div className="mt-1 line-clamp-2 text-xs text-bad" title={b.last.error}>
                  {b.last.error}
                </div>
              )}
              {b.last.status === "failed" && (
                <Link
                  to={`/logs?job=${encodeURIComponent(b.last.id)}`}
                  className="mt-1 inline-flex items-center gap-1 text-xs font-medium text-accent-strong hover:underline"
                >
                  <ScrollText className="size-3.5" />
                  See what went wrong in the log
                </Link>
              )}
            </>
          ) : (
            <>
              <div className="mt-0.5 text-sm font-medium">None yet</div>
              <div className="text-xs text-muted">The first build creates the map.</div>
            </>
          )}
        </div>
      </div>
    </Card>
  );
}
