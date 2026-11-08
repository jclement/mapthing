import {
  CheckCircle2,
  ChevronDown,
  Circle,
  CircleDashed,
  Hammer,
  Loader2,
  MinusCircle,
  XCircle,
} from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { api, type Generation, type Job, type LogLine, type Stage, type StageName } from "../api";
import {
  Badge,
  Button,
  Card,
  cx,
  Empty,
  ErrorNote,
  Page,
  PageHeader,
  Progress,
  Spinner,
  Stat,
  Switch,
} from "../components/ui";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import { useApi } from "../lib/useApi";

const STAGE_LABEL: Record<StageName, string> = {
  download: "Download extracts",
  merge: "Merge",
  basemap: "Vector basemap",
  routing: "Routing graph",
  elevation: "Elevation tiles",
  verify: "Verify",
  swap: "Go live",
};

function secs(a?: string, b?: string) {
  if (!a) return undefined;
  return ((b ? Date.parse(b) : Date.now()) - Date.parse(a)) / 1000;
}

function StageIcon({ s }: { s: Stage["status"] }) {
  const c = "size-4 shrink-0";
  if (s === "done") return <CheckCircle2 className={cx(c, "text-ok")} />;
  if (s === "running") return <Loader2 className={cx(c, "animate-spin text-accent")} />;
  if (s === "failed") return <XCircle className={cx(c, "text-bad")} />;
  if (s === "skipped") return <MinusCircle className={cx(c, "text-faint")} />;
  return <CircleDashed className={cx(c, "text-faint")} />;
}

function JobStatus({ j }: { j: Job }) {
  const map = {
    queued: ["info", "Queued"],
    running: ["accent", "Running"],
    succeeded: ["ok", "Succeeded"],
    failed: ["bad", "Failed"],
    cancelled: ["neutral", "Cancelled"],
  } as const;
  const [tone, label] = map[j.status];
  return (
    <Badge tone={tone} dot>
      {label}
    </Badge>
  );
}

function Stages({ job }: { job: Job }) {
  return (
    <ol className="space-y-2.5">
      {job.stages.map((s) => {
        const d = secs(s.started_at, s.finished_at);
        return (
          <li key={s.name} className="grid grid-cols-[1rem_1fr_auto] items-center gap-x-3">
            <StageIcon s={s.status} />
            <div className="min-w-0">
              <div className="flex items-center justify-between gap-2 text-[13px]">
                <span className={cx(s.status === "pending" && "text-muted")}>
                  {STAGE_LABEL[s.name]}
                </span>
                {s.detail && <span className="truncate text-xs text-faint">{s.detail}</span>}
              </div>
              {s.status === "running" && (
                <Progress value={s.progress} live className="mt-1 h-1.5" />
              )}
            </div>
            <span className="tnum w-16 text-right text-xs text-faint">
              {d != null ? fmt.duration(d) : ""}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

function LogTail({ lines }: { lines: LogLine[] }) {
  const ref = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: scroll on new lines
  useEffect(() => {
    const el = ref.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines.length]);
  return (
    <pre
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget;
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      }}
      className="h-64 overflow-auto rounded-lg bg-[#0f141b] p-3 font-mono text-[11.5px] leading-5 text-slate-300"
      role="log"
      aria-label="Build log"
    >
      {lines.length === 0 ? (
        <span className="text-slate-500">Waiting for output…</span>
      ) : (
        lines.map((l, i) => (
          <div key={i} className="whitespace-pre-wrap">
            <span className="text-slate-500">
              {new Date(l.t).toLocaleTimeString(undefined, { hour12: false })}{" "}
            </span>
            <span
              className={cx(
                l.level === "error" && "text-red-400",
                l.level === "warn" && "text-amber-300",
                l.level === "debug" && "text-slate-500",
              )}
            >
              {l.msg}
            </span>
          </div>
        ))
      )}
    </pre>
  );
}

function ActiveJob({ job }: { job: Job }) {
  const { logs } = useEvents();
  const detail = useApi(() => api.job(job.id), [job.id]);
  const lines = useMemo(() => {
    const initial = detail.data?.log ?? [];
    const live = logs[job.id] ?? [];
    const last = initial[initial.length - 1]?.t;
    return initial.concat(last ? live.filter((l) => l.t > last) : live);
  }, [detail.data, logs, job.id]);
  const running = job.stages.find((s) => s.status === "running");
  const elapsed = secs(job.started_at, job.finished_at);
  return (
    <Card
      title={
        <span className="flex items-center gap-2">
          Current build <JobStatus j={job} />
        </span>
      }
      action={
        <span className="text-xs text-faint">
          {job.trigger === "schedule" ? "Scheduled" : "Manual"} · started{" "}
          {fmt.relTime(job.started_at)}
        </span>
      }
    >
      <div className="mb-4">
        <div className="mb-1.5 flex items-baseline justify-between text-sm">
          <span className="font-medium">
            {running
              ? STAGE_LABEL[running.name]
              : job.status === "queued"
                ? "Waiting for the current build"
                : "Finishing"}
          </span>
          <span className="tnum text-muted">
            {fmt.percent(job.progress)}
            {elapsed != null && <span className="text-faint"> · {fmt.duration(elapsed)}</span>}
          </span>
        </div>
        <Progress value={job.progress} live={job.status === "running"} />
      </div>
      <div className="grid gap-5 lg:grid-cols-[minmax(0,320px)_1fr]">
        <Stages job={job} />
        <LogTail lines={lines} />
      </div>
    </Card>
  );
}

function GenerationCard({ g }: { g: Generation }) {
  const m = g.manifest;
  const totalT = Object.values(m.timings).reduce((a, b) => a + (b ?? 0), 0);
  const colors = [
    "var(--color-k1)",
    "var(--color-k2)",
    "var(--color-k3)",
    "var(--color-k4)",
    "var(--color-k5)",
    "var(--color-faint)",
    "var(--color-line-strong)",
  ];
  const okChecks = m.checks.filter((c) => c.ok).length;
  return (
    <div
      className={cx(
        "rounded-xl border p-4",
        g.status === "live" ? "border-accent/40 bg-accent-soft/40" : "border-line",
      )}
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="font-mono text-[13px] font-medium">{g.id}</span>
          {g.status === "live" ? (
            <Badge tone="accent" dot>
              Live
            </Badge>
          ) : (
            <Badge>Kept</Badge>
          )}
        </div>
        <span className="text-xs text-faint">{fmt.dateTime(g.created_at)}</span>
      </div>
      <div className="mt-3 grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Stat label="Total" value={fmt.bytes(m.sizes.total)} />
        <Stat label="Basemap" value={fmt.bytes(m.sizes.basemap)} />
        <Stat label="Routing" value={fmt.bytes(m.sizes.routing)} />
        <Stat label="Terrain" value={fmt.bytes(m.sizes.terrain)} />
      </div>
      <div className="mt-3">
        <div className="mb-1 flex justify-between text-[11px] text-faint">
          <span>Build time</span>
          <span className="tnum">{fmt.duration(totalT)}</span>
        </div>
        <div className="flex h-2 overflow-hidden rounded-full bg-sunken">
          {Object.entries(m.timings).map(([k, v], i) => (
            <div
              key={k}
              title={`${STAGE_LABEL[k as StageName]}: ${fmt.duration(v ?? 0)}`}
              style={{
                width: `${((v ?? 0) / (totalT || 1)) * 100}%`,
                background: colors[i % colors.length],
              }}
            />
          ))}
        </div>
      </div>
      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
        <span>{m.regions.map((r) => r.name).join(", ")}</span>
        <span className={okChecks === m.checks.length ? "text-ok" : "text-bad"}>
          {okChecks}/{m.checks.length} checks passed
        </span>
      </div>
    </div>
  );
}

function HistoryRow({ j }: { j: Job }) {
  const [open, setOpen] = useState(false);
  const d = secs(j.started_at, j.finished_at);
  const failed = j.stages.find((s) => s.status === "failed");
  return (
    <>
      <tr
        className="cursor-pointer border-b border-line hover:bg-sunken/60"
        onClick={() => setOpen((o) => !o)}
      >
        <td className="px-4 py-2.5">
          <JobStatus j={j} />
        </td>
        <td className="px-3 py-2.5">{fmt.dateTime(j.created_at)}</td>
        <td className="px-3 py-2.5 text-muted">
          {j.trigger === "schedule" ? "Scheduled" : "Manual"}
        </td>
        <td className="tnum px-3 py-2.5 text-right">{d != null ? fmt.duration(d) : "—"}</td>
        <td className="px-3 py-2.5">
          {j.error ? (
            <span className="text-bad">{j.error}</span>
          ) : j.generation_id ? (
            <span className="font-mono text-xs text-muted">{j.generation_id}</span>
          ) : null}
        </td>
        <td className="px-3 py-2.5 text-right">
          <ChevronDown
            className={cx("inline size-4 text-faint transition-transform", open && "rotate-180")}
          />
        </td>
      </tr>
      {open && (
        <tr className="border-b border-line bg-sunken/40">
          <td colSpan={6} className="px-4 py-3">
            <div className="max-w-md">
              <Stages job={j} />
            </div>
            {failed && (
              <p className="mt-2 text-xs text-muted">
                Failed during {STAGE_LABEL[failed.name]}. The previous generation stayed live.
              </p>
            )}
          </td>
        </tr>
      )}
    </>
  );
}

export function BuildsPage() {
  const { jobs, active, generationTick } = useEvents();
  const gens = useApi(() => api.generations(), [generationTick]);
  const regions = useApi(() => api.regions(), [generationTick]);
  const [refresh, setRefresh] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const list = Object.values(jobs).sort((a, b) => b.created_at.localeCompare(a.created_at));
  const queued = list.filter((j) => j.status === "queued" && j.id !== active?.id);
  const history = list.filter((j) => j.status !== "running" && j.status !== "queued");
  const noRegions = regions.data && regions.data.regions.length === 0;

  async function start() {
    setBusy(true);
    setError(undefined);
    try {
      await api.build(refresh);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Page>
      <PageHeader
        title="Builds"
        description="Each build combines every region into a new generation, checks it, then swaps it in."
        actions={
          <>
            <div className="flex items-center gap-2 text-xs text-muted">
              <Switch checked={refresh} onChange={setRefresh} label="Refresh downloads" />
              Refresh downloads
            </div>
            <Button
              variant="primary"
              icon={<Hammer className="size-4" />}
              loading={busy}
              disabled={!!noRegions}
              onClick={start}
            >
              {active ? "Queue build" : "Build now"}
            </Button>
          </>
        }
      />
      {error && (
        <div className="mb-4">
          <ErrorNote error={error} />
        </div>
      )}
      <div className="space-y-5">
        {active ? (
          <ActiveJob job={active} />
        ) : (
          <Card>
            <div className="flex items-center gap-3 text-sm text-muted">
              <Circle className="size-4 text-ok" />
              {noRegions
                ? "Add a region first, then build."
                : "Idle. The next build runs on the schedule, or start one now."}
            </div>
          </Card>
        )}
        {queued.length > 0 && (
          <p className="text-xs text-muted">
            {queued.length} more build{queued.length > 1 ? "s" : ""} queued.
          </p>
        )}

        <Card title="Generations">
          {gens.loading && !gens.data ? (
            <Spinner />
          ) : gens.error ? (
            <ErrorNote error={gens.error} onRetry={gens.reload} />
          ) : gens.data?.generations.length ? (
            <div className="grid gap-3 lg:grid-cols-2">
              {gens.data.generations.map((g) => (
                <GenerationCard key={g.id} g={g} />
              ))}
            </div>
          ) : (
            <Empty title="No generations yet">The first successful build creates one.</Empty>
          )}
        </Card>

        <Card title="History" pad={false}>
          {history.length === 0 ? (
            <Empty title="No finished builds yet" />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full min-w-[640px] text-sm">
                <thead>
                  <tr className="border-b border-line text-left text-[11px] uppercase tracking-wide text-faint">
                    <th className="px-4 py-2 font-medium">Result</th>
                    <th className="px-3 py-2 font-medium">Started</th>
                    <th className="px-3 py-2 font-medium">Trigger</th>
                    <th className="px-3 py-2 text-right font-medium">Duration</th>
                    <th className="px-3 py-2 font-medium">Outcome</th>
                    <th className="px-3 py-2" />
                  </tr>
                </thead>
                <tbody>
                  {history.map((j) => (
                    <HistoryRow key={j.id} j={j} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </Page>
  );
}
