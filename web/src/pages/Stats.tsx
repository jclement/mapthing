import { Link } from "react-router";
import { api, type RequestKind } from "../api";
import { BarList, type Series, StackedBars } from "../components/charts";
import { Badge, Card, Empty, ErrorNote, Page, PageHeader, Spinner, Stat } from "../components/ui";
import * as fmt from "../lib/format";
import { useApi } from "../lib/useApi";

const KINDS: (Series & { key: RequestKind })[] = [
  { key: "tiles", label: "Vector tiles", color: "var(--color-k1)" },
  { key: "raster", label: "Raster tiles", color: "var(--color-k2)" },
  { key: "elevation", label: "Elevation", color: "var(--color-k3)" },
  { key: "routes", label: "Routes", color: "var(--color-k4)" },
  { key: "packs", label: "Offline packs", color: "var(--color-k5)" },
];

export function StatsPage() {
  const s = useApi(() => api.stats(), [], 30000);
  const d = s.data;

  if (!d) {
    return (
      <Page>
        <PageHeader title="Stats" />
        {s.error ? <ErrorNote error={s.error} onRetry={s.reload} /> : <Spinner />}
      </Page>
    );
  }

  const totals = KINDS.map((k) => ({ ...k, value: d.daily.reduce((a, r) => a + r[k.key], 0) }));
  const all = totals.reduce((a, t) => a + t.value, 0);
  const last7 = d.daily.slice(-7).reduce((a, r) => a + KINDS.reduce((b, k) => b + r[k.key], 0), 0);
  const prev7 = d.daily
    .slice(-14, -7)
    .reduce((a, r) => a + KINDS.reduce((b, k) => b + r[k.key], 0), 0);
  const trend = prev7 ? (last7 - prev7) / prev7 : 0;
  const diskUsed = d.disk.total_bytes - d.disk.free_bytes;
  const mt =
    d.disk.generations.reduce((a, g) => a + g.bytes, 0) +
    d.disk.downloads_bytes +
    d.disk.elevation_cache_bytes +
    d.disk.imagery_cache_bytes;

  return (
    <Page wide>
      <PageHeader
        title="Stats"
        description={
          <>
            Requests over the last 30 days, by kind and by key, with caches and disk. Live rates,
            latency, memory and processes are on the{" "}
            <Link to="/" className="text-accent-strong hover:underline">
              Overview
            </Link>
            .
          </>
        }
      />
      <div className="mb-5 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Card>
          <Stat
            label="Requests (30 d)"
            value={fmt.number(all)}
            sub={`${fmt.number(last7)} in the last 7 days`}
          />
        </Card>
        <Card>
          <Stat
            label="Week over week"
            value={
              <span className={trend >= 0 ? "text-ok" : "text-bad"}>
                {trend >= 0 ? "+" : ""}
                {fmt.percent(trend)}
              </span>
            }
            sub="vs the 7 days before"
          />
        </Card>
        <Card>
          <Stat
            label="mapthing on disk"
            value={fmt.bytes(mt)}
            sub={`${fmt.bytes(d.disk.free_bytes)} free of ${fmt.bytes(d.disk.total_bytes)}`}
          />
        </Card>
        <Card>
          <Stat
            label="Busiest day"
            value={fmt.number(
              Math.max(0, ...d.daily.map((r) => KINDS.reduce((b, k) => b + r[k.key], 0))),
            )}
            sub="requests in one day"
          />
        </Card>
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-3 [&>*]:min-w-0">
        <Card title="Requests by kind" className="lg:col-span-2">
          {d.daily.length ? (
            <StackedBars
              rows={d.daily}
              series={KINDS}
              xLabel={(r) =>
                new Date(`${r.date}T00:00:00Z`).toLocaleDateString(undefined, {
                  month: "short",
                  day: "numeric",
                  timeZone: "UTC",
                })
              }
            />
          ) : (
            <Empty title="No requests yet" />
          )}
        </Card>
        <Card title="By key (14 d)">
          {d.by_key.length ? (
            <BarList
              items={[...d.by_key]
                .sort((a, b) => b.requests - a.requests)
                .map((k) => ({ key: k.key_id, label: k.label, value: k.requests }))}
              format={fmt.number}
            />
          ) : (
            <Empty title="No keyed traffic yet" />
          )}
        </Card>

        <Card title="Caches">
          <ul className="space-y-3.5">
            {d.cache.map((c) => {
              const rate = c.hits / Math.max(1, c.hits + c.misses);
              return (
                <li key={c.name}>
                  <div className="mb-1 flex items-baseline justify-between text-[13px]">
                    <span>{c.name}</span>
                    <span className="tnum font-medium">{fmt.percent(rate, 1)}</span>
                  </div>
                  <div className="h-1.5 overflow-hidden rounded-full bg-sunken">
                    <div
                      className="h-full rounded-full bg-accent"
                      style={{ width: fmt.percent(rate) }}
                    />
                  </div>
                  <div className="tnum mt-1 text-xs text-faint">
                    {fmt.number(c.hits)} hits · {fmt.number(c.misses)} misses ·{" "}
                    {fmt.bytes(c.size_bytes)}
                  </div>
                </li>
              );
            })}
          </ul>
        </Card>

        <Card title="Disk">
          <div className="mb-3">
            <div className="flex h-2.5 overflow-hidden rounded-full bg-sunken">
              <div
                className="bg-accent"
                style={{ width: `${(mt / d.disk.total_bytes) * 100}%` }}
                title="mapthing"
              />
              <div
                className="bg-line-strong"
                style={{ width: `${((diskUsed - mt) / d.disk.total_bytes) * 100}%` }}
                title="other"
              />
            </div>
            <div className="mt-1.5 flex justify-between text-xs text-muted">
              <span>
                <span className="mr-1 inline-block size-2 rounded-sm bg-accent" />
                mapthing {fmt.bytes(mt)}
              </span>
              <span>{fmt.bytes(d.disk.free_bytes)} free</span>
            </div>
          </div>
          <dl className="space-y-1.5 text-[13px]">
            {d.disk.generations.map((g, i) => (
              <div key={g.id} className="flex justify-between gap-3">
                <dt className="truncate">
                  <span className="font-mono text-xs">{g.id}</span>{" "}
                  {i === 0 && <Badge tone="accent">live</Badge>}
                </dt>
                <dd className="tnum text-muted">{fmt.bytes(g.bytes)}</dd>
              </div>
            ))}
            <div className="flex justify-between border-t border-line pt-1.5">
              <dt>Downloads</dt>
              <dd className="tnum text-muted">{fmt.bytes(d.disk.downloads_bytes)}</dd>
            </div>
            <div className="flex justify-between">
              <dt>Elevation cache</dt>
              <dd className="tnum text-muted">{fmt.bytes(d.disk.elevation_cache_bytes)}</dd>
            </div>
            <div className="flex justify-between">
              <dt>Imagery cache</dt>
              <dd className="tnum text-muted">{fmt.bytes(d.disk.imagery_cache_bytes)}</dd>
            </div>
          </dl>
          {d.disk.regions.length > 0 && (
            <>
              <div className="mt-4 mb-1.5 text-[11px] font-medium uppercase tracking-wide text-faint">
                By region
              </div>
              <BarList
                items={d.disk.regions.map((r) => ({ key: r.id, label: r.name, value: r.bytes }))}
                format={(v) => fmt.bytes(v)}
              />
            </>
          )}
        </Card>
      </div>
    </Page>
  );
}
