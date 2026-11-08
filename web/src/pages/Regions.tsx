import {
  ChevronRight,
  Globe2,
  Hammer,
  Layers,
  Plus,
  Search,
  SquareDashed,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { lazy, Suspense, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import {
  api,
  type BBox,
  type Catalog,
  type CatalogEntry,
  type ElevationSource,
  type NewRegion,
  type Region,
  type Schedule,
} from "../api";
import {
  Badge,
  Button,
  Card,
  Confirm,
  cx,
  Empty,
  ErrorNote,
  Field,
  Input,
  Modal,
  Page,
  PageHeader,
  Segmented,
  Spinner,
  Switch,
} from "../components/ui";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import { bboxAreaKm2, bboxContains, estimateDisk } from "../lib/geo";
import { useApi } from "../lib/useApi";

const DrawBox = lazy(() => import("../components/DrawBox").then((m) => ({ default: m.DrawBox })));

export function RegionsPage() {
  const { generationTick, active } = useEvents();
  const regions = useApi(() => api.regions(), [generationTick, active?.status]);
  const catalog = useApi(() => api.catalog(), []);
  const schedule = useApi(() => api.schedule(), []);
  const system = useApi(() => api.system(), [generationTick]);
  const [params, setParams] = useSearchParams();
  const adding = params.has("add");
  const [removing, setRemoving] = useState<Region | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const nav = useNavigate();

  const list = regions.data?.regions ?? [];
  const pending = list.filter((r) => r.status === "pending");

  async function buildNow() {
    setBusy(true);
    try {
      await api.build(true);
      nav("/builds");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function toggleRefresh(id: string, enabled: boolean) {
    if (!schedule.data) return;
    const next: Schedule = {
      ...schedule.data,
      regions: schedule.data.regions.some((r) => r.id === id)
        ? schedule.data.regions.map((r) => (r.id === id ? { ...r, enabled } : r))
        : [...schedule.data.regions, { id, enabled }],
    };
    schedule.setData(() => next);
    try {
      await api.saveSchedule(next);
    } catch (e) {
      setError((e as Error).message);
      schedule.reload();
    }
  }

  async function remove() {
    if (!removing) return;
    setBusy(true);
    try {
      await api.removeRegion(removing.id);
      setRemoving(null);
      regions.reload();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Page>
      <PageHeader
        title="Regions"
        description="Areas downloaded and combined into one basemap, routing graph and elevation set."
        actions={
          <Button
            variant="primary"
            icon={<Plus className="size-4" />}
            onClick={() => setParams({ add: "1" })}
          >
            Add region
          </Button>
        }
      />
      {error && (
        <div className="mb-4">
          <ErrorNote error={error} />
        </div>
      )}

      {pending.length > 0 && (
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-accent/30 bg-accent-soft px-4 py-3">
          <div className="text-sm">
            <span className="font-medium">{pending.map((r) => r.name).join(", ")}</span>{" "}
            <span className="text-muted">
              {pending.length === 1 ? "is" : "are"} added but not built yet.
              {active ? " A build is in progress." : ""}
            </span>
          </div>
          {!active && (
            <Button
              variant="primary"
              size="sm"
              loading={busy}
              icon={<Hammer className="size-3.5" />}
              onClick={buildNow}
            >
              Build now
            </Button>
          )}
        </div>
      )}

      <Card pad={false} title="Installed">
        {regions.loading && !regions.data ? (
          <Spinner />
        ) : regions.error ? (
          <div className="p-4">
            <ErrorNote error={regions.error} onRetry={regions.reload} />
          </div>
        ) : list.length === 0 ? (
          <Empty
            icon={<Layers className="size-5" />}
            title="No regions yet"
            action={
              <Button
                variant="primary"
                icon={<Plus className="size-4" />}
                onClick={() => setParams({ add: "1" })}
              >
                Add a region
              </Button>
            }
          >
            Start with the smallest area you need. You can add more later; every build combines all
            regions so routes cross borders cleanly.
          </Empty>
        ) : (
          <RegionTable
            regions={list}
            sources={catalog.data?.elevation_sources ?? []}
            schedule={schedule.data}
            onToggle={toggleRefresh}
            onRemove={setRemoving}
          />
        )}
      </Card>

      {list.length > 0 && system.data && (
        <p className="mt-3 text-xs text-faint">
          {fmt.bytes(list.reduce((s, r) => s + r.disk_bytes, 0))} used by regions ·{" "}
          {fmt.bytes(system.data.disk.free_bytes)} free on the data volume
        </p>
      )}

      <AddRegion
        open={adding}
        onClose={() => setParams({})}
        catalog={catalog.data}
        catalogError={catalog.error}
        installed={list}
        freeBytes={system.data?.disk.free_bytes}
        hasData={!!system.data?.current_generation}
        onAdded={async (build) => {
          setParams({});
          regions.reload();
          schedule.reload();
          if (build && !active) await buildNow();
        }}
      />

      <Confirm
        open={!!removing}
        title={`Remove ${removing?.name}?`}
        confirmLabel="Remove"
        busy={busy}
        onClose={() => setRemoving(null)}
        onConfirm={remove}
      >
        Its download is deleted now. The region stays in the live map until the next build finishes
        without it.
      </Confirm>
    </Page>
  );
}

function RegionTable({
  regions,
  sources,
  schedule,
  onToggle,
  onRemove,
}: {
  regions: Region[];
  sources: ElevationSource[];
  schedule: Schedule | undefined;
  onToggle: (id: string, v: boolean) => void;
  onRemove: (r: Region) => void;
}) {
  const srcName = (id: string) => sources.find((s) => s.id === id);
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[720px] text-sm">
        <thead>
          <tr className="border-b border-line text-left text-[11px] font-medium uppercase tracking-wide text-faint">
            <th className="px-4 py-2 font-medium">Region</th>
            <th className="px-3 py-2 font-medium">Status</th>
            <th className="px-3 py-2 text-right font-medium">Disk</th>
            <th className="px-3 py-2 font-medium">Data date</th>
            <th className="px-3 py-2 font-medium">Elevation</th>
            <th className="px-3 py-2 font-medium">Refresh</th>
            <th className="px-3 py-2" />
          </tr>
        </thead>
        <tbody>
          {regions.map((r) => {
            const age = fmt.ageDays(r.data_date);
            const enabled = schedule?.regions.find((x) => x.id === r.id)?.enabled ?? true;
            return (
              <tr key={r.id} className="border-b border-line last:border-0 hover:bg-sunken/60">
                <td className="px-4 py-2.5">
                  <div className="font-medium">{r.name}</div>
                  <div className="text-xs text-faint">
                    {r.catalog_id ?? `custom box ${r.bbox.map((v) => v.toFixed(2)).join(", ")}`}
                  </div>
                </td>
                <td className="px-3 py-2.5">
                  {r.status === "installed" ? (
                    <Badge tone="ok" dot>
                      Installed
                    </Badge>
                  ) : r.status === "pending" ? (
                    <Badge tone="info" dot>
                      Not built
                    </Badge>
                  ) : (
                    <Badge tone="bad" dot>
                      Failed
                    </Badge>
                  )}
                </td>
                <td className="tnum px-3 py-2.5 text-right">
                  {r.disk_bytes ? (
                    fmt.bytes(r.disk_bytes)
                  ) : (
                    <span className="text-faint">≈ {fmt.bytes(r.extract_bytes * 4.5)}</span>
                  )}
                </td>
                <td className="px-3 py-2.5">
                  {r.data_date ? (
                    <div className="flex items-center gap-1.5">
                      <span>{fmt.date(r.data_date)}</span>
                      <span
                        className={cx(
                          "text-xs",
                          age != null && age > 14 ? "text-warn" : "text-faint",
                        )}
                      >
                        {fmt.relTime(r.data_date)}
                      </span>
                    </div>
                  ) : (
                    <span className="text-faint">—</span>
                  )}
                </td>
                <td className="px-3 py-2.5">
                  <div className="flex flex-wrap gap-1">
                    {r.elevation_sources.map((s) => {
                      const info = srcName(s);
                      return (
                        <Badge key={s} tone="neutral">
                          {info ? `${info.name} · ${info.resolution_m} m` : s}
                        </Badge>
                      );
                    })}
                  </div>
                </td>
                <td className="px-3 py-2.5">
                  <Switch
                    checked={enabled}
                    disabled={!schedule}
                    onChange={(v) => onToggle(r.id, v)}
                    label={`Refresh ${r.name} on schedule`}
                  />
                </td>
                <td className="px-3 py-2.5 text-right">
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label={`Remove ${r.name}`}
                    onClick={() => onRemove(r)}
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ---------------- add region ----------------

type Selection =
  | { kind: "catalog"; entry: CatalogEntry }
  | { kind: "box"; name: string; bbox: BBox };

function estimateFor(sel: Selection, catalog: Catalog) {
  const srcById = (id: string) => catalog.elevation_sources.find((s) => s.id === id);
  if (sel.kind === "catalog") {
    const area = bboxAreaKm2(sel.entry.bbox);
    const sources = sel.entry.elevation_sources.map(srcById).filter(Boolean) as ElevationSource[];
    // Terrain is built at the national/regional grid; 1 m sources only where small enough.
    const usable = sources.filter((s) => s.resolution_m >= 10 || area < 5000);
    const best = Math.min(...usable.map((s) => s.resolution_m), 30);
    return {
      area,
      sources,
      est: estimateDisk({ areaKm2: area, extractBytes: sel.entry.extract_bytes, bestResM: best }),
    };
  }
  const area = bboxAreaKm2(sel.bbox);
  const parent = catalog.entries
    .filter((e) => e.extract_bytes > 0 && bboxContains(e.bbox, sel.bbox))
    .sort((a, b) => bboxAreaKm2(a.bbox) - bboxAreaKm2(b.bbox))[0];
  const density = parent ? parent.extract_bytes / bboxAreaKm2(parent.bbox) : 4000;
  const sources = (parent?.elevation_sources ?? ["copernicus-glo30"])
    .map(srcById)
    .filter(Boolean) as ElevationSource[];
  const best = Math.min(
    ...sources.filter((s) => s.resolution_m >= 10 || area < 5000).map((s) => s.resolution_m),
    30,
  );
  return {
    area,
    sources,
    parent,
    est: estimateDisk({ areaKm2: area, extractBytes: density * area, bestResM: best }),
  };
}

function AddRegion({
  open,
  onClose,
  catalog,
  catalogError,
  installed,
  freeBytes,
  hasData,
  onAdded,
}: {
  open: boolean;
  onClose: () => void;
  catalog: Catalog | undefined;
  catalogError: Error | undefined;
  installed: Region[];
  freeBytes: number | undefined;
  hasData: boolean;
  onAdded: (build: boolean) => void;
}) {
  const [tab, setTab] = useState<"catalog" | "draw">("catalog");
  const [sel, setSel] = useState<Selection | null>(null);
  const [boxName, setBoxName] = useState("");
  const [box, setBox] = useState<BBox | null>(null);
  const [busy, setBusy] = useState<"add" | "build" | null>(null);
  const [error, setError] = useState<string>();

  useEffect(() => {
    if (!open) {
      setSel(null);
      setBox(null);
      setBoxName("");
      setError(undefined);
      setTab("catalog");
    }
  }, [open]);

  useEffect(() => {
    if (tab === "draw") setSel(box ? { kind: "box", name: boxName, bbox: box } : null);
  }, [tab, box, boxName]);

  const summary = useMemo(
    () => (sel && catalog ? estimateFor(sel, catalog) : null),
    [sel, catalog],
  );
  const installedIds = new Set(installed.map((r) => r.id));
  const alreadyInstalled = sel?.kind === "catalog" && installedIds.has(sel.entry.id);
  const canAdd =
    !!sel &&
    !alreadyInstalled &&
    (sel.kind === "catalog" || (sel.name.trim().length > 0 && !!sel.bbox));

  async function add(build: boolean) {
    if (!sel) return;
    setBusy(build ? "build" : "add");
    setError(undefined);
    try {
      const body: NewRegion =
        sel.kind === "catalog" ? { id: sel.entry.id } : { name: sel.name.trim(), bbox: sel.bbox };
      await api.addRegion(body);
      onAdded(build);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(null);
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Add a region"
      size="lg"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button disabled={!canAdd} loading={busy === "add"} onClick={() => add(false)}>
            Add only
          </Button>
          <Button
            variant="primary"
            disabled={!canAdd}
            loading={busy === "build"}
            icon={<Hammer className="size-4" />}
            onClick={() => add(true)}
          >
            Add and build
          </Button>
        </>
      }
    >
      <div className="mb-3">
        <Segmented
          label="How to pick"
          value={tab}
          onChange={(t) => {
            setTab(t);
            setSel(null);
          }}
          options={[
            {
              value: "catalog",
              label: (
                <>
                  <Globe2 className="size-3.5" /> From catalog
                </>
              ),
            },
            {
              value: "draw",
              label: (
                <>
                  <SquareDashed className="size-3.5" /> Draw a box
                </>
              ),
            },
          ]}
        />
      </div>

      {/* A custom box does not need the catalog (it only refines the estimate), so only the
          catalog tab waits for it or shows its error. */}
      {tab === "catalog" && catalogError && <ErrorNote error={catalogError} />}
      {tab === "catalog" && !catalog && !catalogError && <Spinner label="Loading catalog" />}

      {tab === "catalog" ? (
        catalog && (
          <CatalogTree
            catalog={catalog}
            installed={installedIds}
            selected={sel?.kind === "catalog" ? sel.entry.id : null}
            onSelect={(entry) => setSel({ kind: "catalog", entry })}
          />
        )
      ) : (
        <div className="space-y-3">
          <Field label="Name" hint="Shown in the region list, e.g. “Permian core”">
            {(id) => (
              <Input
                id={id}
                value={boxName}
                onChange={(e) => setBoxName(e.target.value)}
                placeholder="My area"
              />
            )}
          </Field>
          <div className="h-72">
            <Suspense fallback={<Spinner label="Loading map" />}>
              <DrawBox onChange={setBox} hasData={hasData} />
            </Suspense>
          </div>
          <BBoxInputs value={box} onChange={setBox} />
        </div>
      )}

      {sel && summary && (
        <div className="mt-4 rounded-xl border border-line bg-sunken p-4" data-testid="estimate">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <div className="font-medium">
              {sel.kind === "catalog" ? sel.entry.name : sel.name || "Custom box"}
            </div>
            <div className="text-xs text-muted">
              {Math.round(summary.area).toLocaleString()} km²
            </div>
          </div>
          {alreadyInstalled ? (
            <p className="mt-2 text-sm text-muted">Already installed.</p>
          ) : (
            <>
              <div className="mt-3 grid grid-cols-2 gap-3 sm:grid-cols-5">
                {(
                  [
                    ["Download", summary.est.download],
                    ["Basemap", summary.est.basemap],
                    ["Routing", summary.est.routing],
                    ["Terrain", summary.est.terrain],
                    ["Total", summary.est.total],
                  ] as const
                ).map(([label, v]) => (
                  <div key={label}>
                    <div className="text-[11px] text-faint">{label}</div>
                    <div className={cx("tnum text-sm", label === "Total" && "font-semibold")}>
                      ≈ {fmt.bytes(v)}
                    </div>
                  </div>
                ))}
              </div>
              {freeBytes != null && summary.est.total * 2 > freeBytes && (
                <p className="mt-3 flex items-center gap-1.5 text-xs text-warn">
                  <TriangleAlert className="size-3.5" />
                  Builds need room for the new generation beside the live one; only{" "}
                  {fmt.bytes(freeBytes)} is free.
                </p>
              )}
              <div className="mt-3">
                <div className="mb-1 text-[11px] text-faint">Elevation sources (best first)</div>
                <div className="flex flex-wrap gap-1">
                  {summary.sources.map((s) => (
                    <Badge key={s.id} tone="accent">
                      {s.name} · {s.resolution_m} m
                    </Badge>
                  ))}
                </div>
              </div>
              {sel.kind === "box" && (
                <p className="mt-3 text-xs text-faint">
                  {"parent" in summary && summary.parent
                    ? `Clipped from ${summary.parent.name}.`
                    : "No catalog extract covers this box completely; it is clipped from the smallest extracts that do."}
                </p>
              )}
            </>
          )}
        </div>
      )}
      {error && (
        <div className="mt-3">
          <ErrorNote error={error} />
        </div>
      )}
    </Modal>
  );
}

function BBoxInputs({
  value,
  onChange,
}: {
  value: BBox | null;
  onChange: (b: BBox | null) => void;
}) {
  const [text, setText] = useState("");
  // Only rewrite the text when the box comes from elsewhere (drawn on the map), never while
  // the user is typing: "…, 33" is already a valid box, and reformatting it would eat ".1".
  const typed = useRef<BBox | null>(null);
  useEffect(() => {
    if (value && value !== typed.current) setText(value.map((v) => v.toFixed(4)).join(", "));
  }, [value]);
  const parsed = text
    .split(/[\s,]+/)
    .filter(Boolean)
    .map(Number);
  const valid =
    parsed.length === 4 &&
    parsed.every(Number.isFinite) &&
    parsed[0] < parsed[2] &&
    parsed[1] < parsed[3] &&
    Math.abs(parsed[1]) <= 90 &&
    Math.abs(parsed[3]) <= 90;
  return (
    <Field
      label="Bounding box (west, south, east, north)"
      error={text && !valid ? "Four numbers: west, south, east, north" : undefined}
      hint="Drawn boxes fill this in; you can also type or paste one."
    >
      {(id) => (
        <Input
          id={id}
          value={text}
          placeholder="-104.2, 31.0, -101.2, 33.2"
          className="font-mono text-xs"
          onChange={(e) => {
            setText(e.target.value);
            const p = e.target.value
              .split(/[\s,]+/)
              .filter(Boolean)
              .map(Number);
            const ok = p.length === 4 && p.every(Number.isFinite) && p[0] < p[2] && p[1] < p[3];
            typed.current = ok ? (p as BBox) : null;
            onChange(typed.current);
          }}
        />
      )}
    </Field>
  );
}

function CatalogTree({
  catalog,
  installed,
  selected,
  onSelect,
}: {
  catalog: Catalog;
  installed: Set<string>;
  selected: string | null;
  onSelect: (e: CatalogEntry) => void;
}) {
  const [q, setQ] = useState("");
  const [open, setOpen] = useState<Set<string>>(() => new Set(["north-america"]));
  const children = useMemo(() => {
    const m = new Map<string | null, CatalogEntry[]>();
    for (const e of catalog.entries) {
      const arr = m.get(e.parent) ?? [];
      arr.push(e);
      m.set(e.parent, arr);
    }
    for (const arr of m.values()) arr.sort((a, b) => a.name.localeCompare(b.name));
    return m;
  }, [catalog]);

  const needle = q.trim().toLowerCase();
  const matches = useMemo(() => {
    if (!needle) return null;
    const keep = new Set<string>();
    const byId = new Map(catalog.entries.map((e) => [e.id, e]));
    for (const e of catalog.entries) {
      if (e.name.toLowerCase().includes(needle) || e.id.toLowerCase().includes(needle)) {
        let cur: CatalogEntry | undefined = e;
        while (cur) {
          keep.add(cur.id);
          cur = cur.parent ? byId.get(cur.parent) : undefined;
        }
      }
    }
    return keep;
  }, [needle, catalog]);

  const render = (parent: string | null, depth: number) =>
    (children.get(parent) ?? [])
      .filter((e) => !matches || matches.has(e.id))
      .map((e) => {
        const kids = children.get(e.id);
        const isOpen = matches ? true : open.has(e.id);
        const selectable = e.extract_bytes > 0;
        const isInstalled = installed.has(e.id);
        return (
          <li key={e.id}>
            <div
              className={cx(
                "flex items-center gap-1 rounded-lg pr-2 text-[13px]",
                selected === e.id ? "bg-accent-soft text-accent-strong" : "hover:bg-sunken",
              )}
              style={{ paddingLeft: depth * 16 + 4 }}
            >
              {kids ? (
                <button
                  type="button"
                  aria-label={isOpen ? `Collapse ${e.name}` : `Expand ${e.name}`}
                  onClick={() =>
                    setOpen((s) => {
                      const n = new Set(s);
                      if (n.has(e.id)) n.delete(e.id);
                      else n.add(e.id);
                      return n;
                    })
                  }
                  className="grid size-6 place-items-center rounded text-faint hover:text-fg"
                >
                  <ChevronRight
                    className={cx("size-3.5 transition-transform", isOpen && "rotate-90")}
                  />
                </button>
              ) : (
                <span className="size-6" />
              )}
              <button
                type="button"
                disabled={!selectable}
                onClick={() => onSelect(e)}
                className={cx(
                  "flex min-w-0 flex-1 items-center justify-between gap-2 py-1.5 text-left",
                  !selectable && "cursor-default font-medium text-muted",
                )}
              >
                <span className="truncate">{e.name}</span>
                <span className="flex shrink-0 items-center gap-2">
                  {isInstalled && <Badge tone="ok">installed</Badge>}
                  {selectable && (
                    <span className="tnum text-xs text-faint">{fmt.bytes(e.extract_bytes)}</span>
                  )}
                </span>
              </button>
            </div>
            {kids && isOpen && <ul>{render(e.id, depth + 1)}</ul>}
          </li>
        );
      });

  return (
    <div>
      <div className="relative mb-2">
        <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-faint" />
        <Input
          aria-label="Search regions"
          placeholder="Search: Texas, Alberta, Norway…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          className="pl-9"
        />
      </div>
      <ul
        className="max-h-72 overflow-y-auto rounded-xl border border-line p-1"
        aria-label="Region catalog"
      >
        {render(null, 0)}
        {matches && matches.size === 0 && (
          <li className="px-3 py-6 text-center text-sm text-faint">Nothing matches “{q}”</li>
        )}
      </ul>
    </div>
  );
}
