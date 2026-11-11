import { ArrowDown, Download, Pause, Play, Search, X } from "lucide-react";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import { api, type LogEntry, type LogPage, logsDownloadUrl, subscribeLogs } from "../api";
import {
  Badge,
  Button,
  cx,
  ErrorNote,
  Input,
  PageHeader,
  Segmented,
  Spinner,
} from "../components/ui";
import * as fmt from "../lib/format";
import {
  apiQuery,
  filterToSearch,
  follows,
  fromLocalInput,
  type LogFilter,
  lineTime,
  mergeEntries,
  parseFilter,
  type RangeKey,
  sourceSlot,
  toLocalInput,
} from "../lib/logs";

const PAGE = 500; // lines per request
const CAP = 3000; // lines kept in the view

const RANGE_LABEL: Record<RangeKey, string> = {
  live: "Latest",
  "1h": "Last hour",
  "24h": "Last 24 hours",
  all: "Everything kept",
  custom: "Custom range",
};

// Source colours on the dark log pane (fixed: the pane is dark in both themes).
const SOURCE_COLOR = [
  "text-teal-300",
  "text-sky-300",
  "text-amber-300",
  "text-fuchsia-300",
  "text-lime-300",
  "text-orange-300",
];

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

export function LogsPage() {
  const [params, setParams] = useSearchParams();
  const [filter, setFilterState] = useState<LogFilter>(() => parseFilter(params));
  const [text, setText] = useState(filter.q);
  const q = useDebounced(text, 300);

  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [meta, setMeta] = useState<Pick<LogPage, "sources" | "store">>();
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [error, setError] = useState<Error>();
  const [connected, setConnected] = useState(false);
  const [paused, setPaused] = useState(false);
  const [held, setHeld] = useState(0); // lines that arrived while paused
  const [lost, setLost] = useState(0); // lines the server dropped because we fell behind
  const [atBottom, setAtBottom] = useState(true);
  const [reloadTick, setReloadTick] = useState(0);

  const pausedRef = useRef(paused);
  pausedRef.current = paused;
  const heldRef = useRef<LogEntry[]>([]);
  const pane = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const keepTop = useRef<number | null>(null); // scroll height before older lines were added

  const filterRef = useRef(filter);
  filterRef.current = filter;
  const setFilter = useCallback(
    (patch: Partial<LogFilter>) => {
      const next = { ...filterRef.current, ...patch };
      filterRef.current = next;
      setFilterState(next);
      setParams(filterToSearch(next), { replace: true });
    },
    [setParams],
  );
  useEffect(() => {
    if (q !== filter.q) setFilter({ q });
  }, [q, filter.q, setFilter]);

  const live = follows(filter);
  const filterKey = JSON.stringify(filter);

  // Load the newest page for the filter, then follow the stream from its last line.
  // biome-ignore lint/correctness/useExhaustiveDependencies: filterKey stands for filter
  useEffect(() => {
    let alive = true;
    let off: (() => void) | undefined;
    setLoading(true);
    setError(undefined);
    setHeld(0);
    setLost(0);
    heldRef.current = [];
    stick.current = true;
    setAtBottom(true);
    const query = apiQuery(filter, { limit: String(PAGE) });
    api
      .logs(query)
      .then((page) => {
        if (!alive) return;
        setEntries(page.entries);
        setMore(page.more);
        setMeta({ sources: page.sources, store: page.store });
        setLoading(false);
        if (!follows(filter)) return;
        // "1" replays everything the server still has in memory when the page was empty.
        const after = page.entries[page.entries.length - 1]?.id ?? "1";
        off = subscribeLogs(apiQuery(filter, { after }), {
          entry: (e) => {
            if (pausedRef.current) {
              heldRef.current.push(e);
              setHeld(heldRef.current.length);
            } else {
              setEntries((cur) => mergeEntries(cur, [e], CAP));
            }
          },
          dropped: (n) => setLost((x) => x + n),
          state: setConnected,
        });
      })
      .catch((e: Error) => {
        if (!alive) return;
        setError(e);
        setLoading(false);
      });
    return () => {
      alive = false;
      off?.();
      setConnected(false);
    };
  }, [filterKey, reloadTick]);

  function resume() {
    const batch = heldRef.current;
    heldRef.current = [];
    setHeld(0);
    setPaused(false);
    stick.current = true;
    setEntries((cur) => mergeEntries(cur, batch, CAP));
  }

  async function loadOlder() {
    const first = entries[0];
    if (!first) return;
    setLoadingOlder(true);
    try {
      const page = await api.logs(apiQuery(filter, { limit: String(PAGE), before: first.id }));
      keepTop.current = pane.current?.scrollHeight ?? null;
      stick.current = false;
      setEntries((cur) => mergeEntries(cur, page.entries, CAP * 4, "old"));
      setMore(page.more);
    } catch (e) {
      setError(e as Error);
    } finally {
      setLoadingOlder(false);
    }
  }

  // Follow the tail while the reader is at the bottom; keep their place when older lines load.
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs when the lines change
  useLayoutEffect(() => {
    const el = pane.current;
    if (!el) return;
    if (keepTop.current != null) {
      el.scrollTop += el.scrollHeight - keepTop.current;
      keepTop.current = null;
    } else if (stick.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [entries]);

  const sources = useMemo(() => {
    const all = new Set([...(meta?.sources ?? []), ...filter.sources]);
    return [...all];
  }, [meta, filter.sources]);

  const store = meta?.store;
  const now = new Date();
  const downloadQuery = apiQuery(filter);

  return (
    <div className="flex h-full flex-col">
      <div className="mx-auto flex min-h-0 w-full max-w-7xl flex-1 flex-col px-4 py-6 sm:px-6 lg:px-8">
        <PageHeader
          title="Logs"
          description={
            store ? (
              <>
                The server, its processes and builds, in one place. Keeping {fmt.bytes(store.bytes)}{" "}
                of {fmt.bytes(store.max_bytes, 0)}
                {store.oldest && <>, back to {fmt.dateTime(store.oldest)}</>}.
              </>
            ) : (
              "The server, its processes and builds, in one place."
            )
          }
          actions={
            <>
              {live &&
                (paused ? (
                  <Button variant="primary" icon={<Play className="size-4" />} onClick={resume}>
                    Resume{held > 0 ? ` (${fmt.number(held)} new)` : ""}
                  </Button>
                ) : (
                  <Button icon={<Pause className="size-4" />} onClick={() => setPaused(true)}>
                    Pause
                  </Button>
                ))}
              <a
                href={logsDownloadUrl(downloadQuery)}
                download
                className="inline-flex h-9 items-center justify-center gap-1.5 rounded-lg border border-line-strong bg-panel px-3.5 text-sm font-medium whitespace-nowrap text-fg shadow-xs hover:bg-sunken"
                title="Every line matching the filters, as text"
              >
                <Download className="size-4" />
                Download
              </a>
              <a
                href={logsDownloadUrl(downloadQuery, "jsonl")}
                download
                className="inline-flex h-9 items-center rounded-lg px-2 text-xs font-medium text-muted hover:bg-sunken hover:text-fg"
                title="The same lines as JSON, one object per line"
              >
                JSON
              </a>
            </>
          }
        />

        {/* Filters */}
        <div className="mb-3 space-y-2.5 rounded-(--radius-card) border border-line bg-panel p-3 shadow-xs">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-40 flex-1">
              <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-faint" />
              <Input
                aria-label="Search log text"
                placeholder="Search text"
                value={text}
                onChange={(e) => setText(e.target.value)}
                className="pl-8"
              />
            </div>
            <Segmented
              label="Level"
              value={filter.level}
              onChange={(level) => setFilter({ level })}
              options={[
                { value: "", label: "All", title: "Every level" },
                { value: "info", label: "Info", title: "Info, warnings and errors" },
                { value: "warn", label: "Warn", title: "Warnings and errors" },
                { value: "error", label: "Error", title: "Errors only" },
              ]}
            />
            <select
              aria-label="Time range"
              value={filter.range}
              onChange={(e) => {
                const range = e.target.value as RangeKey;
                setFilter(
                  range === "custom"
                    ? {
                        range,
                        since: filter.since || new Date(Date.now() - 3600_000).toISOString(),
                      }
                    : { range, since: "", until: "" },
                );
              }}
              className="h-9 rounded-lg border border-line-strong bg-panel px-2.5 text-sm text-fg shadow-xs"
            >
              {(Object.keys(RANGE_LABEL) as RangeKey[]).map((k) => (
                <option key={k} value={k}>
                  {RANGE_LABEL[k]}
                </option>
              ))}
            </select>
          </div>
          {filter.range === "custom" && (
            <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
              <label className="flex items-center gap-1.5">
                From
                <input
                  type="datetime-local"
                  aria-label="From"
                  value={toLocalInput(filter.since)}
                  min={store?.oldest ? toLocalInput(store.oldest) : undefined}
                  onChange={(e) => setFilter({ since: fromLocalInput(e.target.value) })}
                  className="h-8 rounded-lg border border-line-strong bg-panel px-2 text-[13px] text-fg"
                />
              </label>
              <label className="flex items-center gap-1.5">
                to
                <input
                  type="datetime-local"
                  aria-label="To"
                  value={toLocalInput(filter.until)}
                  onChange={(e) => setFilter({ until: fromLocalInput(e.target.value) })}
                  className="h-8 rounded-lg border border-line-strong bg-panel px-2 text-[13px] text-fg"
                />
              </label>
              <span className="text-faint">
                {filter.until ? "" : "No end: new lines keep arriving."}
                {store?.oldest && ` The log goes back to ${fmt.dateTime(store.oldest)}.`}
              </span>
            </div>
          )}
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="mr-1 text-xs text-faint">Sources</span>
            <SourceChip
              label="All"
              on={filter.sources.length === 0}
              onClick={() => setFilter({ sources: [] })}
            />
            {sources.map((s) => {
              const on = filter.sources.includes(s);
              return (
                <SourceChip
                  key={s}
                  label={s}
                  on={on}
                  onClick={() =>
                    setFilter({
                      sources: on ? filter.sources.filter((x) => x !== s) : [...filter.sources, s],
                    })
                  }
                />
              );
            })}
            {filter.job && (
              <span className="ml-1 inline-flex items-center gap-1 rounded-md bg-accent-soft py-0.5 pr-1 pl-2 text-xs font-medium text-accent-strong">
                Build #{filter.job}
                <button
                  type="button"
                  aria-label="Show every build"
                  onClick={() => setFilter({ job: "" })}
                  className="rounded p-0.5 hover:bg-accent/15"
                >
                  <X className="size-3" />
                </button>
              </span>
            )}
          </div>
        </div>

        {error && (
          <div className="mb-3">
            <ErrorNote error={error} onRetry={() => setReloadTick((x) => x + 1)} />
          </div>
        )}
        {store?.file_error && (
          <div className="mb-3">
            <ErrorNote
              error={`Log files are not being written (${store.file_error}): only lines since the server started are kept.`}
            />
          </div>
        )}

        {/* Lines */}
        <div className="relative min-h-64 flex-1">
          <div
            ref={pane}
            role="log"
            aria-label="Server log"
            aria-live="off"
            onScroll={(e) => {
              const el = e.currentTarget;
              const bottom = el.scrollHeight - el.scrollTop - el.clientHeight < 32;
              stick.current = bottom;
              setAtBottom(bottom);
            }}
            className="absolute inset-0 overflow-auto rounded-(--radius-card) bg-[#0f141b] py-2 font-mono text-[11.5px] leading-5 text-slate-300"
          >
            {loading ? (
              <div className="text-slate-400">
                <Spinner label="Loading the log" />
              </div>
            ) : entries.length === 0 ? (
              <div className="px-4 py-10 text-center font-sans text-sm text-slate-400">
                {error ? (
                  "The log could not be loaded."
                ) : (
                  <>
                    <div className="font-medium text-slate-200">No lines match</div>
                    <div className="mt-1 text-slate-500">
                      {live
                        ? "New lines appear here as they are written. Widen the filters to see more."
                        : "Nothing was logged in that range with these filters."}
                    </div>
                  </>
                )}
              </div>
            ) : (
              <>
                <div className="px-3 pb-1 font-sans text-xs text-slate-500">
                  {more ? (
                    <button
                      type="button"
                      onClick={loadOlder}
                      disabled={loadingOlder}
                      className="rounded-md border border-slate-700 px-2 py-0.5 text-slate-300 hover:bg-slate-800 disabled:opacity-50"
                    >
                      {loadingOlder ? "Loading…" : "Load older lines"}
                    </button>
                  ) : (
                    "Start of the matching lines."
                  )}
                </div>
                {entries.map((e) => (
                  <Line key={e.id} e={e} now={now} showJob={!filter.job} />
                ))}
              </>
            )}
          </div>
          {!atBottom && !loading && entries.length > 0 && (
            <button
              type="button"
              onClick={() => {
                const el = pane.current;
                if (el) el.scrollTop = el.scrollHeight;
              }}
              className="absolute right-4 bottom-3 inline-flex items-center gap-1 rounded-full border border-slate-600 bg-slate-800/95 px-3 py-1 text-xs font-medium text-slate-100 shadow-float hover:bg-slate-700"
            >
              <ArrowDown className="size-3.5" />
              Latest
            </button>
          )}
        </div>
        <div className="mt-2 flex flex-wrap items-center justify-between gap-2 text-xs text-muted">
          <span className="tnum">
            {fmt.number(entries.length)} line{entries.length === 1 ? "" : "s"} shown
            {lost > 0 && (
              <span className="text-warn">
                {" "}
                · {fmt.number(lost)} skipped (arriving faster than the page can show; they are in
                the download)
              </span>
            )}
          </span>
          {live ? (
            paused ? (
              <Badge tone="warn" dot>
                Paused
              </Badge>
            ) : connected ? (
              <Badge tone="ok" dot>
                Live
              </Badge>
            ) : (
              <Badge tone="neutral" dot>
                {loading ? "Connecting" : "Reconnecting"}
              </Badge>
            )
          ) : (
            <Badge>Past range</Badge>
          )}
        </div>
      </div>
    </div>
  );
}

function SourceChip({ label, on, onClick }: { label: string; on: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      aria-pressed={on}
      onClick={onClick}
      className={cx(
        "rounded-md px-2 py-0.5 font-mono text-xs ring-1 ring-inset transition-colors",
        on
          ? "bg-accent-soft text-accent-strong ring-accent/30"
          : "bg-sunken text-muted ring-line hover:text-fg",
      )}
    >
      {label}
    </button>
  );
}

function Line({ e, now, showJob }: { e: LogEntry; now: Date; showJob: boolean }) {
  return (
    <div
      className={cx(
        "flex flex-wrap gap-x-2 px-3 hover:bg-white/5 sm:flex-nowrap",
        e.level === "error" && "bg-red-500/10",
        e.level === "warn" && "bg-amber-400/5",
      )}
    >
      <span className="shrink-0 text-slate-500" title={new Date(e.t).toISOString()}>
        {lineTime(e.t, now)}
      </span>
      <span
        className={cx(
          "w-10 shrink-0 uppercase",
          e.level === "error"
            ? "text-red-400"
            : e.level === "warn"
              ? "text-amber-300"
              : "text-slate-500",
        )}
      >
        {e.level === "error" ? "err" : e.level === "debug" ? "dbg" : e.level}
      </span>
      <span className={cx("shrink-0", SOURCE_COLOR[sourceSlot(e.source)])}>
        {e.source}
        {showJob && e.job ? ` #${e.job}` : ""}
      </span>
      <span
        className={cx(
          "min-w-0 basis-full break-words whitespace-pre-wrap sm:basis-auto",
          e.level === "error" && "text-red-200",
          e.level === "warn" && "text-amber-100",
          e.level === "debug" && "text-slate-500",
        )}
      >
        {e.msg}
      </span>
    </div>
  );
}
