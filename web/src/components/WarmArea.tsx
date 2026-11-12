// "Warm an area": fill the on-demand cache for a region or a box ahead of time, so it keeps
// working with no internet. An estimate first, then a job with progress that can be stopped.
import { Download, Square } from "lucide-react";
import { useEffect, useState } from "react";
import { api, type OnDemandStatus, type WarmEstimate, type WarmRequest } from "../api";
import * as fmt from "../lib/format";
import { estimateSummary, parseBox, warmSummary } from "../lib/ondemand";
import { Badge, Button, cx, ErrorNote, Input, Progress } from "./ui";

const ESTIMATE_DEBOUNCE_MS = import.meta.env.MODE === "test" ? 0 : 400;
const BOX = "__box__";

export function WarmArea({
  status,
  names,
  onChange,
  reload,
}: {
  status: OnDemandStatus;
  names: Record<string, string>;
  onChange: (s: OnDemandStatus) => void;
  reload: () => void;
}) {
  const warm = status.warm;
  const running = warm?.state === "running";
  const boxAllowed = status.config.outside === "on_demand";
  const [area, setArea] = useState(status.regions[0] ?? BOX);
  const [boxText, setBoxText] = useState("");
  const [zoom, setZoom] = useState(status.maxzoom);
  const [est, setEst] = useState<{ key: string; est?: WarmEstimate; error?: string }>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  // The chosen region may have been switched back to built terrain meanwhile.
  useEffect(() => {
    if (area !== BOX && !status.regions.includes(area)) setArea(status.regions[0] ?? BOX);
  }, [area, status.regions]);

  const box = area === BOX ? parseBox(boxText) : null;
  const req: WarmRequest | null =
    area === BOX ? (box ? { bbox: box, max_zoom: zoom } : null) : { region: area, max_zoom: zoom };
  const key = req ? JSON.stringify(req) : "";

  // Ask for the estimate whenever the request changes, and again when a job ends.
  // biome-ignore lint/correctness/useExhaustiveDependencies: key stands for req
  useEffect(() => {
    if (!req || running) return;
    let live = true;
    const t = setTimeout(() => {
      api
        .warmEstimate(req)
        .then((e) => live && setEst({ key, est: e }))
        .catch((e: Error) => live && setEst({ key, error: e.message }));
    }, ESTIMATE_DEBOUNCE_MS);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [key, running]);

  // While a job runs, follow it.
  useEffect(() => {
    if (!running) return;
    const t = setInterval(reload, import.meta.env.MODE === "test" ? 20 : 1500);
    return () => clearInterval(t);
  }, [running, reload]);

  const now = est && est.key === key ? est : undefined;
  const canStart = !!req && !running && !!now?.est && now.est.applies && now.est.fits;

  async function start() {
    if (!req) return;
    setBusy(true);
    setError(undefined);
    try {
      await api.warmStart(req);
      reload();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function cancel() {
    setBusy(true);
    try {
      onChange(await api.warmCancel());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!status.enabled) {
    return (
      <p className="text-xs text-faint">
        Once on-demand terrain is on for a region or outside built regions, an area can be fetched
        ahead of time here, so it keeps working with no internet.
      </p>
    );
  }
  const select =
    "h-9 rounded-lg border border-line-strong bg-panel px-2.5 text-sm text-fg shadow-xs outline-none focus:border-accent focus:ring-3 focus:ring-accent/20";
  return (
    <div className="space-y-3">
      <p className="max-w-3xl text-xs text-muted">
        Fetches every on-demand tile of an area now, down to the zoom you choose, so the area keeps
        working with no internet: terrain, contours, elevation, and offline packs cut from it. Built
        terrain is left alone. Warmed tiles are ordinary cache: they stay as long as the cache has
        room for them.
      </p>
      <div className="flex flex-wrap items-end gap-3">
        <div className="space-y-1.5">
          <label htmlFor="warm-area" className="block text-xs font-medium text-muted">
            Area
          </label>
          <select
            id="warm-area"
            className={select}
            value={area}
            disabled={running}
            onChange={(e) => setArea(e.target.value)}
          >
            {status.regions.map((id) => (
              <option key={id} value={id}>
                {names[id] ?? id}
              </option>
            ))}
            {boxAllowed && <option value={BOX}>A box (west, south, east, north)</option>}
          </select>
        </div>
        {area === BOX && (
          <div className="min-w-0 flex-1 space-y-1.5 sm:max-w-xs">
            <label htmlFor="warm-box" className="block text-xs font-medium text-muted">
              Box, in degrees
            </label>
            <Input
              id="warm-box"
              value={boxText}
              disabled={running}
              onChange={(e) => setBoxText(e.target.value)}
              placeholder="-104.2, 31.0, -101.2, 33.2"
            />
          </div>
        )}
        <div className="space-y-1.5">
          <label htmlFor="warm-zoom" className="block text-xs font-medium text-muted">
            Down to zoom
          </label>
          <select
            id="warm-zoom"
            className={select}
            value={zoom}
            disabled={running}
            onChange={(e) => setZoom(Number(e.target.value))}
          >
            {Array.from(
              { length: status.maxzoom - status.minzoom + 1 },
              (_, i) => status.maxzoom - i,
            ).map((z) => (
              <option key={z} value={z}>
                {z}
                {z === status.maxzoom ? " (full detail, and elevation)" : ""}
              </option>
            ))}
          </select>
        </div>
        {running ? (
          <Button icon={<Square className="size-3.5" />} loading={busy} onClick={cancel}>
            Stop
          </Button>
        ) : (
          <Button
            variant="primary"
            icon={<Download className="size-4" />}
            disabled={!canStart}
            loading={busy}
            onClick={start}
          >
            Warm this area
          </Button>
        )}
      </div>

      {running && warm ? (
        <div data-testid="warm-progress">
          <div className="text-[13px] font-medium">{warmSummary(warm)}</div>
          <Progress value={warm.total ? warm.done / warm.total : 0} live className="mt-1.5 h-1.5" />
          <div className="tnum mt-1 text-xs text-muted">
            {fmt.number(warm.fetched)} fetched · {fmt.number(warm.cached)} were cached
            {warm.failed > 0 ? ` · ${fmt.number(warm.failed)} failed` : ""}
            {warm.started_at ? ` · started ${fmt.relTime(warm.started_at)}` : ""}
          </div>
        </div>
      ) : (
        <>
          {area === BOX && boxText.trim() !== "" && !box && (
            <p className="text-xs text-bad">
              Give four numbers: west, south, east, north (west less than east, south less than
              north).
            </p>
          )}
          {now?.error && <p className="text-xs text-bad">{now.error}</p>}
          {now?.est && (
            <p
              data-testid="warm-estimate"
              className={cx("text-[13px]", now.est.applies ? "text-fg" : "text-muted")}
            >
              {estimateSummary(now.est)}
            </p>
          )}
          {now?.est?.applies && !now.est.fits && (
            <p className="text-xs text-warn">
              The cache cannot hold this area: it would push out its own first tiles. Raise the
              cache size to at least {fmt.bytes((now.est.need_cache_mb ?? 0) * 1e6)}, or choose a
              smaller area or a lower zoom.
            </p>
          )}
          {warm && warm.state !== "idle" && (
            <p
              className="flex flex-wrap items-center gap-2 text-xs text-muted"
              data-testid="warm-last"
            >
              <Badge
                tone={warm.state === "done" ? "ok" : warm.state === "failed" ? "bad" : "neutral"}
                dot
              >
                {warm.state === "done" ? "done" : warm.state === "failed" ? "failed" : "stopped"}
              </Badge>
              <span>
                {warmSummary(warm)}
                {warm.finished_at ? `, ${fmt.relTime(warm.finished_at)}` : ""}.
                {warm.failed > 0 && warm.state !== "failed"
                  ? ` ${fmt.number(warm.failed)} could not be fetched.`
                  : ""}
              </span>
              {warm.error && <span className="text-bad">{warm.error}</span>}
            </p>
          )}
        </>
      )}
      {error && <ErrorNote error={error} />}
    </div>
  );
}
