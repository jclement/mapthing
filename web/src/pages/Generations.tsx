import { ArrowRight, History, Layers, RotateCcw, Trash2, Zap } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router";
import { ApiError, api, type Generation } from "../api";
import {
  Badge,
  Button,
  Card,
  Confirm,
  cx,
  Empty,
  ErrorNote,
  Page,
  PageHeader,
  Spinner,
  Stat,
} from "../components/ui";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import { activateLabel, STATUS, sharedBytes, sizeOf, whyNot } from "../lib/generations";
import { useApi } from "../lib/useApi";
import { stageLabel } from "./Builds";

const TIMING_COLORS = [
  "var(--color-k1)",
  "var(--color-k2)",
  "var(--color-k3)",
  "var(--color-k4)",
  "var(--color-k5)",
  "var(--color-faint)",
  "var(--color-line-strong)",
];

type Pending = { g: Generation; action: "activate" | "delete" };

export function GenerationsPage() {
  const { generationTick, active } = useEvents();
  const list = useApi(() => api.generations(), [generationTick], active ? 10000 : 60000);
  const [pending, setPending] = useState<Pending>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ message: string; problems?: string[] }>();
  const [notice, setNotice] = useState<string>();

  const gens = list.data?.generations ?? [];
  const disk = list.data?.disk;
  const live = gens.find((g) => g.status === "live");
  const shared = sharedBytes(disk);

  async function confirm() {
    if (!pending) return;
    const { g, action } = pending;
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      if (action === "delete") {
        await api.deleteGeneration(g.id);
        setNotice(`Generation ${g.id} was deleted.`);
      } else {
        const res = await api.activateGeneration(g.id);
        setNotice(
          res.already_live
            ? `Generation ${g.id} was already live.`
            : `Generation ${g.id} is live.${res.previous ? ` ${res.previous} is kept for rollback.` : ""}${
                res.removed.length ? ` Removed: ${res.removed.join(", ")}.` : ""
              }`,
        );
      }
      list.reload();
    } catch (e) {
      const err = e as Error;
      setError({
        message: err.message,
        problems: err instanceof ApiError ? err.problems : undefined,
      });
    } finally {
      setBusy(false);
      setPending(undefined);
    }
  }

  return (
    <Page>
      <PageHeader
        title="Generations"
        description="Each build makes a generation: one complete copy of the map, routing graph and elevation. One is live; the one before it is kept so you can roll back."
        actions={
          <Link
            to="/builds"
            className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-line-strong bg-panel px-3.5 text-sm font-medium text-fg shadow-xs hover:bg-sunken"
          >
            Builds <ArrowRight className="size-4" />
          </Link>
        }
      />

      {error && (
        <div className="mb-4 rounded-lg border border-bad/30 bg-bad/8 px-3 py-2 text-sm text-bad">
          <div>{error.message}</div>
          {error.problems && error.problems.length > 0 && (
            <ul className="mt-1 list-disc pl-5 text-[13px]">
              {error.problems.map((p) => (
                <li key={p}>{p}</li>
              ))}
            </ul>
          )}
        </div>
      )}
      {notice && !error && (
        <output className="mb-4 block rounded-lg border border-ok/30 bg-ok/8 px-3 py-2 text-sm text-ok">
          {notice}
        </output>
      )}

      {list.loading && !list.data ? (
        <Spinner />
      ) : list.error && !list.data ? (
        <ErrorNote error={list.error} onRetry={list.reload} />
      ) : gens.length === 0 ? (
        <Card>
          <Empty
            icon={<Layers className="size-5" />}
            title="No generations yet"
            action={
              <Link
                to="/regions"
                className="text-sm font-medium text-accent-strong hover:underline"
              >
                Choose regions and build
              </Link>
            }
          >
            The first successful build creates one and makes it live. A generation built on another
            machine and copied into the data folder also appears here.
          </Empty>
        </Card>
      ) : (
        <div className="space-y-5">
          <Card>
            <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
              <Stat
                label="Generations"
                value={gens.length}
                sub={live ? `live: ${live.id}` : "none live"}
              />
              <Stat
                label="On disk"
                value={fmt.bytes(disk?.bytes ?? gens.reduce((a, g) => a + sizeOf(g), 0))}
                sub={shared > 0 ? `${fmt.bytes(shared)} shared between them` : "all generations"}
              />
              <Stat
                label="Free"
                value={disk ? fmt.bytes(disk.free_bytes) : "—"}
                sub={disk ? `of ${fmt.bytes(disk.total_bytes)}` : undefined}
              />
              <Stat
                label="Rollback"
                value={gens.some((g) => g.status === "previous") ? "Ready" : "None"}
                sub={
                  gens.some((g) => g.status === "previous")
                    ? "previous generation kept"
                    : "after the next build"
                }
              />
            </div>
            {shared > 0 && (
              <p className="mt-3 text-xs text-faint">
                Terrain that did not change is shared between generations instead of being stored
                twice, so the sizes below add up to more than the disk they use.
              </p>
            )}
          </Card>

          <ul className="space-y-3">
            {gens.map((g) => (
              <li key={g.id}>
                <GenerationRow
                  g={g}
                  live={live}
                  onActivate={() => setPending({ g, action: "activate" })}
                  onDelete={() => setPending({ g, action: "delete" })}
                />
              </li>
            ))}
          </ul>
          <p className="text-xs text-faint">
            After each successful build or activation the live generation and the newest other
            complete one are kept; older ones this server built are deleted. Generations copied in
            from another machine are never deleted automatically.
          </p>
        </div>
      )}

      <Confirm
        open={pending?.action === "delete"}
        title="Delete this generation?"
        confirmLabel="Delete generation"
        busy={busy}
        onConfirm={confirm}
        onClose={() => setPending(undefined)}
      >
        {pending && (
          <>
            <span className="font-mono text-fg">{pending.g.id}</span> (
            {fmt.bytes(sizeOf(pending.g))}) will be removed from disk. This cannot be undone.
            {pending.g.status === "previous" &&
              " It is the generation kept for rollback: until the next build there will be nothing to roll back to."}
          </>
        )}
      </Confirm>
      <Confirm
        open={pending?.action === "activate"}
        title={pending ? `${activateLabel(pending.g, live)}?` : ""}
        confirmLabel={pending ? activateLabel(pending.g, live) : ""}
        variant="primary"
        busy={busy}
        onConfirm={confirm}
        onClose={() => setPending(undefined)}
      >
        {pending && (
          <>
            Tiles, routing and elevation switch to{" "}
            <span className="font-mono text-fg">{pending.g.id}</span> without downtime. It is
            checked first; if anything is wrong the live generation keeps serving.
            {live && (
              <>
                {" "}
                <span className="font-mono text-fg">{live.id}</span> stays on disk, so you can
                switch back.
              </>
            )}
          </>
        )}
      </Confirm>
    </Page>
  );
}

function GenerationRow({
  g,
  live,
  onActivate,
  onDelete,
}: {
  g: Generation;
  live: Generation | undefined;
  onActivate: () => void;
  onDelete: () => void;
}) {
  const m = g.manifest;
  const st = STATUS[g.status] ?? STATUS.kept;
  const totalT = Object.values(m.timings).reduce((a, b) => a + (b ?? 0), 0);
  const okChecks = m.checks.filter((c) => c.ok).length;
  const label = activateLabel(g, live);
  const noDelete = whyNot(g, "delete");
  return (
    <section
      aria-label={`Generation ${g.id}`}
      className={cx(
        "rounded-(--radius-card) border bg-panel p-4 shadow-xs",
        g.status === "live" ? "border-accent/40 bg-accent-soft/40" : "border-line",
        g.status === "invalid" && "border-bad/30",
      )}
    >
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-[13px] font-medium">{g.id}</span>
            <Badge tone={st.tone} dot={g.status === "live" || g.status === "building"}>
              {st.label}
            </Badge>
          </div>
          <div className="mt-0.5 text-xs text-muted">
            {g.status === "building" ? "Started" : "Built"} {fmt.dateTime(g.created_at)} (
            {fmt.relTime(g.created_at)}) · {st.what}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {g.status === "building" && (
            <Link to="/builds" className="text-xs font-medium text-accent-strong hover:underline">
              Watch the build
            </Link>
          )}
          {g.can_activate && (
            <Button
              size="sm"
              icon={
                label === "Make live" ? (
                  <Zap className="size-3.5" />
                ) : (
                  <RotateCcw className="size-3.5" />
                )
              }
              onClick={onActivate}
              aria-label={`${label}: ${g.id}`}
            >
              {label}
            </Button>
          )}
          {g.status !== "live" && g.status !== "building" && (
            <Button
              size="sm"
              variant="ghost"
              icon={<Trash2 className="size-3.5" />}
              disabled={!!noDelete}
              title={noDelete}
              onClick={onDelete}
              aria-label={`Delete ${g.id}`}
            >
              Delete
            </Button>
          )}
        </div>
      </div>

      {g.problems && g.problems.length > 0 && (
        <ul className="mt-3 list-disc space-y-0.5 rounded-lg bg-bad/8 py-2 pr-3 pl-7 text-[13px] text-bad">
          {g.problems.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
      )}

      <div className="mt-3 grid grid-cols-2 gap-3 sm:grid-cols-5">
        <Stat label="On disk" value={fmt.bytes(sizeOf(g))} />
        <Stat label="Basemap" value={m.sizes.basemap ? fmt.bytes(m.sizes.basemap) : "—"} />
        <Stat label="Routing" value={m.sizes.routing ? fmt.bytes(m.sizes.routing) : "—"} />
        <Stat label="Terrain" value={m.sizes.terrain ? fmt.bytes(m.sizes.terrain) : "—"} />
        <Stat label="Contours" value={m.sizes.contours ? fmt.bytes(m.sizes.contours) : "—"} />
      </div>

      {totalT > 0 && (
        <div className="mt-3">
          <div className="mb-1 flex justify-between text-[11px] text-faint">
            <span>Build time</span>
            <span className="tnum">{fmt.duration(totalT)}</span>
          </div>
          <div className="flex h-2 overflow-hidden rounded-full bg-sunken">
            {Object.entries(m.timings).map(([k, v], i) => (
              <div
                key={k}
                title={`${stageLabel(k)}: ${fmt.duration(v ?? 0)}`}
                style={{
                  width: `${((v ?? 0) / totalT) * 100}%`,
                  background: TIMING_COLORS[i % TIMING_COLORS.length],
                }}
              />
            ))}
          </div>
        </div>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted">
        {m.regions.length > 0 ? (
          <span>
            <History className="mr-1 inline size-3.5 text-faint" />
            {m.regions.map((r) => r.name).join(", ")}
          </span>
        ) : (
          <span className="text-faint">Regions not recorded yet</span>
        )}
        {m.checks.length > 0 && (
          <span className={okChecks === m.checks.length ? "text-ok" : "text-bad"}>
            {okChecks}/{m.checks.length} checks passed
          </span>
        )}
        {g.owned === false && g.status !== "building" && (
          <span className="text-faint">not built on this server</span>
        )}
      </div>
    </section>
  );
}
