import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import type { ElevationSample } from "../api";
import { distance as fmtDist, elevation as fmtElev, number as fmtNum } from "../lib/format";
import { cx } from "./ui";

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [w, setW] = useState(600);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(([e]) => setW(Math.max(120, e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, w] as const;
}

function niceStep(range: number, target: number) {
  const raw = range / Math.max(1, target);
  const mag = 10 ** Math.floor(Math.log10(raw || 1));
  const n = raw / mag;
  return (n < 1.5 ? 1 : n < 3 ? 2 : n < 7 ? 5 : 10) * mag;
}

export function Sparkline({
  values,
  width = 96,
  height = 24,
  className,
}: {
  values: number[];
  width?: number;
  height?: number;
  className?: string;
}) {
  if (values.length < 2) return <span className="text-xs text-faint">—</span>;
  const max = Math.max(1, ...values);
  const step = width / (values.length - 1);
  const pts = values.map((v, i) => [i * step, height - 2 - (v / max) * (height - 4)] as const);
  const line = pts.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`).join("");
  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={cx("text-accent", className)}
      aria-hidden="true"
    >
      <path d={`${line}L${width},${height}L0,${height}Z`} fill="currentColor" opacity={0.12} />
      <path d={line} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinejoin="round" />
    </svg>
  );
}

/** Proportional horizontal bar of named parts (e.g. surface breakdown). */
export function PartsBar({
  parts,
  height = 8,
}: {
  parts: { key: string; label: string; value: number; color: string }[];
  height?: number;
}) {
  const total = parts.reduce((s, p) => s + p.value, 0) || 1;
  return (
    <div>
      <div className="flex w-full overflow-hidden rounded-full bg-sunken" style={{ height }}>
        {parts
          .filter((p) => p.value > 0)
          .map((p) => (
            <div
              key={p.key}
              title={`${p.label}: ${Math.round((p.value / total) * 100)}%`}
              style={{ width: `${(p.value / total) * 100}%`, background: p.color }}
            />
          ))}
      </div>
      <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-muted">
        {parts
          .filter((p) => p.value > 0)
          .map((p) => (
            <span key={p.key} className="inline-flex items-center gap-1">
              <span className="size-2 rounded-sm" style={{ background: p.color }} />
              {p.label}{" "}
              <span className="tnum text-faint">{Math.round((p.value / total) * 100)}%</span>
            </span>
          ))}
      </div>
    </div>
  );
}

export interface Series {
  key: string;
  label: string;
  color: string;
}

/** Stacked daily bars with a hover readout. */
export function StackedBars({
  rows,
  series,
  height = 200,
  xLabel,
}: {
  rows: Record<string, number | string>[];
  series: Series[];
  height?: number;
  xLabel: (row: Record<string, number | string>) => string;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const padL = 40;
  const padB = 20;
  const padT = 8;
  const totals = rows.map((r) => series.reduce((s, k) => s + Number(r[k.key] ?? 0), 0));
  const max = Math.max(1, ...totals);
  const step = niceStep(max, 4);
  const top = Math.ceil(max / step) * step;
  const plotW = width - padL - 4;
  const plotH = height - padB - padT;
  const bw = plotW / Math.max(1, rows.length);
  const y = (v: number) => padT + plotH - (v / top) * plotH;
  const ticks: number[] = [];
  for (let t = 0; t <= top + 1e-9; t += step) ticks.push(t);
  const h = hover != null ? rows[hover] : null;

  return (
    <div ref={ref} className="relative">
      <svg width={width} height={height} role="img" aria-label="Requests per day">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={padL} x2={width - 4} y1={y(t)} y2={y(t)} className="stroke-line" />
            <text
              x={padL - 6}
              y={y(t)}
              dy="0.32em"
              textAnchor="end"
              className="fill-faint text-[10px]"
            >
              {fmtNum(t)}
            </text>
          </g>
        ))}
        {rows.map((r, i) => {
          let acc = 0;
          const x = padL + i * bw + bw * 0.15;
          return (
            <g
              key={i}
              onMouseEnter={() => setHover(i)}
              onMouseLeave={() => setHover(null)}
              opacity={hover == null || hover === i ? 1 : 0.55}
            >
              <rect x={padL + i * bw} y={padT} width={bw} height={plotH} fill="transparent" />
              {series.map((s) => {
                const v = Number(r[s.key] ?? 0);
                const y0 = y(acc);
                acc += v;
                const y1 = y(acc);
                return (
                  <rect
                    key={s.key}
                    x={x}
                    y={y1}
                    width={Math.max(1, bw * 0.7)}
                    height={Math.max(0, y0 - y1)}
                    fill={s.color}
                    rx={1}
                  />
                );
              })}
              {(rows.length <= 16 || i % Math.ceil(rows.length / 10) === 0) && (
                <text
                  x={padL + i * bw + bw / 2}
                  y={height - 5}
                  textAnchor="middle"
                  className="fill-faint text-[10px]"
                >
                  {xLabel(r)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
      {h && hover != null && (
        <div
          className="pointer-events-none absolute top-1 z-10 rounded-lg border border-line bg-panel px-2.5 py-1.5 text-xs shadow-float"
          style={{
            left: Math.min(width - 150, Math.max(0, padL + hover * bw + bw / 2 - 70)),
          }}
        >
          <div className="mb-1 font-medium">{xLabel(h)}</div>
          {series.map((s) => (
            <div key={s.key} className="flex items-center justify-between gap-4">
              <span className="inline-flex items-center gap-1.5 text-muted">
                <span className="size-2 rounded-sm" style={{ background: s.color }} />
                {s.label}
              </span>
              <span className="tnum">{Number(h[s.key] ?? 0).toLocaleString()}</span>
            </div>
          ))}
        </div>
      )}
      <Legend series={series} />
    </div>
  );
}

export function Legend({ series }: { series: Series[] }) {
  return (
    <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted">
      {series.map((s) => (
        <span key={s.key} className="inline-flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm" style={{ background: s.color }} />
          {s.label}
        </span>
      ))}
    </div>
  );
}

/** Ranked list with inline bars. */
export function BarList({
  items,
  format = (v) => v.toLocaleString(),
}: {
  items: { key: string; label: ReactNode; value: number; color?: string }[];
  format?: (v: number) => string;
}) {
  const max = Math.max(1, ...items.map((i) => i.value));
  return (
    <ul className="space-y-1.5">
      {items.map((it) => (
        <li key={it.key} className="relative">
          <div
            className="absolute inset-y-0 left-0 rounded-md bg-accent-soft"
            style={{ width: `${(it.value / max) * 100}%`, background: it.color }}
          />
          <div className="relative flex items-center justify-between gap-3 px-2 py-1 text-[13px]">
            <span className="truncate">{it.label}</span>
            <span className="tnum shrink-0 text-muted">{format(it.value)}</span>
          </div>
        </li>
      ))}
    </ul>
  );
}

/** Elevation profile. Hovering reports the sample index so the map can show a marker. */
export function ElevationChart({
  samples,
  height = 140,
  hoverIndex,
  onHover,
  sourceName = (id) => id,
}: {
  sourceName?: (id: string) => string;
  samples: ElevationSample[];
  height?: number;
  hoverIndex?: number | null;
  onHover?: (i: number | null) => void;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const pts = useMemo(
    () => samples.filter((s) => s.elevation_m != null && s.distance_m != null),
    [samples],
  );
  if (pts.length < 2) {
    return (
      <div className="py-6 text-center text-xs text-faint">No elevation data along this path</div>
    );
  }
  const padL = 44;
  const padR = 8;
  const padT = 8;
  const padB = 20;
  const total = pts[pts.length - 1].distance_m!;
  let lo = Math.min(...pts.map((p) => p.elevation_m!));
  let hi = Math.max(...pts.map((p) => p.elevation_m!));
  if (hi - lo < 20) {
    const mid = (hi + lo) / 2;
    lo = mid - 10;
    hi = mid + 10;
  }
  const yStep = niceStep(hi - lo, 3);
  const y0 = Math.floor(lo / yStep) * yStep;
  const y1 = Math.ceil(hi / yStep) * yStep;
  const plotW = width - padL - padR;
  const plotH = height - padT - padB;
  const xOf = (d: number) => padL + (d / (total || 1)) * plotW;
  const yOf = (e: number) => padT + plotH - ((e - y0) / (y1 - y0 || 1)) * plotH;
  const line = pts
    .map(
      (p, i) =>
        `${i ? "L" : "M"}${xOf(p.distance_m!).toFixed(1)},${yOf(p.elevation_m!).toFixed(1)}`,
    )
    .join("");
  const yt: number[] = [];
  for (let v = y0; v <= y1 + 1e-9; v += yStep) yt.push(v);
  const xStep = niceStep(total, Math.max(2, Math.floor(plotW / 90)));
  const xt: number[] = [];
  for (let v = 0; v <= total + 1e-9; v += xStep) xt.push(v);

  const hi_ = hoverIndex != null ? samples[hoverIndex] : undefined;
  const hp = hi_ && hi_.elevation_m != null && hi_.distance_m != null ? hi_ : undefined;

  const pick = (clientX: number, rect: DOMRect) => {
    const d = ((clientX - rect.left - padL) / plotW) * total;
    // binary search on samples by distance
    let a = 0;
    let b = samples.length - 1;
    while (b - a > 1) {
      const m = (a + b) >> 1;
      if ((samples[m].distance_m ?? 0) < d) a = m;
      else b = m;
    }
    const best =
      Math.abs((samples[a].distance_m ?? 0) - d) < Math.abs((samples[b].distance_m ?? 0) - d)
        ? a
        : b;
    onHover?.(best);
  };

  return (
    <div ref={ref} className="relative select-none">
      <svg
        width={width}
        height={height}
        role="img"
        aria-label="Elevation profile"
        onMouseMove={(e) => pick(e.clientX, e.currentTarget.getBoundingClientRect())}
        onTouchMove={(e) => pick(e.touches[0].clientX, e.currentTarget.getBoundingClientRect())}
        onMouseLeave={() => onHover?.(null)}
        className="touch-none"
      >
        <defs>
          <linearGradient id="elev-fill" x1="0" x2="0" y1="0" y2="1">
            <stop offset="0%" stopColor="var(--color-accent)" stopOpacity={0.35} />
            <stop offset="100%" stopColor="var(--color-accent)" stopOpacity={0.03} />
          </linearGradient>
        </defs>
        {yt.map((v) => (
          <g key={v}>
            <line x1={padL} x2={width - padR} y1={yOf(v)} y2={yOf(v)} className="stroke-line" />
            <text
              x={padL - 6}
              y={yOf(v)}
              dy="0.32em"
              textAnchor="end"
              className="fill-faint text-[10px]"
            >
              {Math.round(v)} m
            </text>
          </g>
        ))}
        {xt.map((v) => (
          <text
            key={v}
            x={xOf(v)}
            y={height - 5}
            textAnchor={v === 0 ? "start" : "middle"}
            className="fill-faint text-[10px]"
          >
            {fmtDist(v)}
          </text>
        ))}
        <path
          d={`${line}L${xOf(total)},${padT + plotH}L${padL},${padT + plotH}Z`}
          fill="url(#elev-fill)"
        />
        <path
          d={line}
          fill="none"
          stroke="var(--color-accent)"
          strokeWidth={1.75}
          strokeLinejoin="round"
        />
        {hp && (
          <g>
            <line
              x1={xOf(hp.distance_m!)}
              x2={xOf(hp.distance_m!)}
              y1={padT}
              y2={padT + plotH}
              className="stroke-fg"
              strokeOpacity={0.35}
              strokeDasharray="3 3"
            />
            <circle
              cx={xOf(hp.distance_m!)}
              cy={yOf(hp.elevation_m!)}
              r={4}
              fill="var(--color-panel)"
              stroke="var(--color-accent)"
              strokeWidth={2}
            />
          </g>
        )}
      </svg>
      {hp && (
        <div
          className="pointer-events-none absolute top-0 rounded-md border border-line bg-panel px-2 py-1 text-[11px] shadow-float"
          style={{ left: Math.min(width - 130, Math.max(padL, xOf(hp.distance_m!) + 8)) }}
        >
          <span className="tnum font-medium">{fmtElev(hp.elevation_m)}</span>
          <span className="text-muted"> · {fmtDist(hp.distance_m!)}</span>
          {hp.source && <div className="text-faint">{sourceName(hp.source)}</div>}
        </div>
      )}
    </div>
  );
}
