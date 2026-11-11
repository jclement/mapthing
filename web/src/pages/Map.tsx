import type { FeatureCollection, LineString } from "geojson";
import {
  ArrowDownRight,
  ArrowUpRight,
  Layers,
  Loader2,
  MapPin,
  Mountain,
  MousePointerClick,
  Plus,
  Route as RouteIcon,
  Trash2,
  Truck,
  X,
} from "lucide-react";
import type { GeoJSONSource, Map as MlMap } from "maplibre-gl";
import * as maplibregl from "maplibre-gl";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import {
  api,
  defaultStyle,
  type ElevationProfile,
  type ElevationSample,
  type LonLat,
  type Region,
  type RouteProfile,
  type RouteResult,
  type StyleName,
  styleUrl,
} from "../api";
import { ElevationChart, PartsBar } from "../components/charts";
import { blankStyle, MapCanvas } from "../components/MapCanvas";
import { StylePicker } from "../components/StylePicker";
import { Button, cx, Progress, Segmented, Switch } from "../components/ui";
import { useEvents } from "../lib/events";
import * as fmt from "../lib/format";
import { bboxOf, bboxPolygon, formatLonLat, lineLength, routeFitPadding } from "../lib/geo";
import { useTheme } from "../lib/theme";
import { useApi } from "../lib/useApi";

type Mode = "inspect" | "route";

const SURFACES = [
  { key: "paved", label: "Paved", color: "var(--color-s-paved)" },
  { key: "gravel", label: "Gravel", color: "var(--color-s-gravel)" },
  { key: "dirt", label: "Dirt", color: "var(--color-s-dirt)" },
  { key: "unknown", label: "Unknown", color: "var(--color-s-unknown)" },
] as const;

function regionsFC(regions: Region[]): FeatureCollection {
  return {
    type: "FeatureCollection",
    features: regions.map((r) => ({
      type: "Feature",
      properties: { id: r.id, name: r.name, status: r.status },
      geometry: { type: "Polygon", coordinates: [r.polygon ?? bboxPolygon(r.bbox)] },
    })),
  };
}

// MapLibre style colours must be hex/rgb (no oklch), so overlays use fixed hex values.
const REGION_COLOR = "#0d9488";
const PENDING_COLOR = "#0ea5e9";

function setOrAdd(m: MlMap, id: string, data: GeoJSON.GeoJSON) {
  const src = m.getSource(id) as GeoJSONSource | undefined;
  if (src) src.setData(data);
  else m.addSource(id, { type: "geojson", data });
}

function ensureLayer(m: MlMap, layer: Parameters<MlMap["addLayer"]>[0]) {
  if (!m.getLayer(layer.id)) m.addLayer(layer);
}

function addOverlays(m: MlMap, d: { regions: FeatureCollection; route: LineString | null }) {
  const empty: FeatureCollection = { type: "FeatureCollection", features: [] };
  setOrAdd(m, "mt-regions", d.regions);
  const pending: maplibregl.FilterSpecification = ["==", ["get", "status"], "pending"];
  const built: maplibregl.FilterSpecification = ["!=", ["get", "status"], "pending"];
  ensureLayer(m, {
    id: "mt-regions-fill",
    type: "fill",
    source: "mt-regions",
    paint: { "fill-color": REGION_COLOR, "fill-opacity": 0.04 },
  });
  ensureLayer(m, {
    id: "mt-regions-line",
    type: "line",
    source: "mt-regions",
    filter: built,
    paint: { "line-color": REGION_COLOR, "line-width": 1.5, "line-opacity": 0.85 },
  });
  ensureLayer(m, {
    id: "mt-regions-pending",
    type: "line",
    source: "mt-regions",
    filter: pending,
    paint: { "line-color": PENDING_COLOR, "line-width": 1.5, "line-dasharray": [2, 2] },
  });
  setOrAdd(m, "mt-route", d.route ?? empty);
  ensureLayer(m, {
    id: "mt-route-casing",
    type: "line",
    source: "mt-route",
    layout: { "line-cap": "round", "line-join": "round" },
    paint: { "line-color": "#ffffff", "line-width": 8, "line-opacity": 0.9 },
  });
  ensureLayer(m, {
    id: "mt-route-line",
    type: "line",
    source: "mt-route",
    layout: { "line-cap": "round", "line-join": "round" },
    paint: { "line-color": "#2563eb", "line-width": 4.5 },
  });
}

export function MapPage() {
  const { dark } = useTheme();
  const { active, generationTick } = useEvents();
  const regions = useApi(() => api.regions(), [generationTick]);
  const gens = useApi(() => api.generations(), [generationTick]);
  const catalog = useApi(() => api.catalog(), []);
  const hasLive = !!gens.data?.generations.some((g) => g.status === "live");
  const installed = regions.data?.regions ?? [];

  const styles = useApi(() => api.styles(), []);
  const [styleChoice, setStyleChoice] = useState<StyleName | null>(null);
  const styleName: StyleName = styleChoice ?? defaultStyle(styles.data, dark);
  const style = useMemo(
    // Until it is known that a map is live, draw nothing: asking a server without one for
    // tiles only produces a screen of 503s.
    () => (!gens.data || !hasLive ? blankStyle(dark) : styleUrl(styleName)),
    [gens.data, hasLive, dark, styleName],
  );

  const [mode, setMode] = useState<Mode>("inspect");
  const [waypoints, setWaypoints] = useState<LonLat[]>([]);
  const [profileName, setProfileName] = useState<RouteProfile>("auto");
  const [avoidUnpaved, setAvoidUnpaved] = useState(false);
  // Road profiles follow the bridge deck by default; off = terrain under bridges (buried pipe).
  const [bridgeDeck, setBridgeDeck] = useState(true);
  const [route, setRoute] = useState<RouteResult | null>(null);
  const [elev, setElev] = useState<ElevationProfile | null>(null);
  const [routing, setRouting] = useState(false);
  const [routeError, setRouteError] = useState<string>();
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const [point, setPoint] = useState<{ at: LonLat; sample?: ElevationSample; loading: boolean }>();

  const mapRef = useRef<MlMap | null>(null);
  const markers = useRef<maplibregl.Marker[]>([]);
  const hoverMarker = useRef<maplibregl.Marker | null>(null);
  const pointMarker = useRef<maplibregl.Marker | null>(null);
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const fitted = useRef(false);
  const routePanel = useRef<HTMLDivElement>(null);
  const lastRouteFit = useRef("");

  // Fit the map to a newly computed route, clear of the route panel (side panel on desktop,
  // bottom sheet on phones). Skipped when the route's extent did not change (e.g. toggling
  // bridge handling), so a map the user has panned is not yanked back. Inspect clicks never
  // move the map.
  const fitRoute = useCallback((coords: LonLat[]) => {
    const m = mapRef.current;
    if (!m || coords.length < 2) return;
    const b = bboxOf(coords);
    const key = b.map((v) => v.toFixed(5)).join(",");
    if (key === lastRouteFit.current) return;
    lastRouteFit.current = key;
    const box = m.getContainer().getBoundingClientRect();
    const el = routePanel.current;
    const pr = el?.getBoundingClientRect();
    let panel = pr
      ? { left: pr.left - box.left, top: pr.top - box.top, width: pr.width, height: pr.height }
      : null;
    // The bottom sheet is still short here (stats and profile render after the route), so pad
    // for the height it grows to: its max-height.
    if (el && panel && panel.width >= box.width * 0.8) {
      // getComputedStyle keeps a percentage max-height as "60%"; resolve it against the map.
      const raw = getComputedStyle(el).maxHeight;
      const max = raw.endsWith("%")
        ? (box.height * Number.parseFloat(raw)) / 100
        : Number.parseFloat(raw);
      if (max > panel.height) panel = { ...panel, top: box.height - max, height: max };
    }
    // 56 px clears the floating mode / style toolbar at the top of the map.
    const padding = routeFitPadding({ width: box.width, height: box.height }, panel, 48, 56);
    m.fitBounds(b, { padding, maxZoom: 15, duration: 600 });
  }, []);

  const sourceName = useCallback(
    (id?: string) => catalog.data?.elevation_sources?.find((s) => s.id === id)?.name ?? id ?? "—",
    [catalog.data],
  );

  // ----- overlays (re-added after every style load) -----
  const data = useRef({ regions: regionsFC([]), route: null as LineString | null });
  data.current.regions = regionsFC(installed);
  data.current.route = route?.geometry ?? null;

  const syncOverlays = useCallback((m: MlMap) => {
    // Not gated on isStyleLoaded(): that stays false while any tile is pending. Adding a
    // source before the style is parsed throws; the style.load handler retries then.
    try {
      addOverlays(m, data.current);
    } catch {
      /* style not ready yet */
    }
  }, []);

  // biome-ignore lint/correctness/useExhaustiveDependencies: data.current changes with these
  useEffect(() => {
    if (mapRef.current) syncOverlays(mapRef.current);
  }, [installed, route, syncOverlays]);

  // Fit to installed regions once.
  useEffect(() => {
    const m = mapRef.current;
    if (!m || fitted.current || installed.length === 0) return;
    fitted.current = true;
    const b = installed.reduce(
      (a, r) => [
        Math.min(a[0], r.bbox[0]),
        Math.min(a[1], r.bbox[1]),
        Math.max(a[2], r.bbox[2]),
        Math.max(a[3], r.bbox[3]),
      ],
      [180, 90, -180, -90],
    );
    m.fitBounds(b as [number, number, number, number], { padding: 60, duration: 0 });
  }, [installed]);

  // ----- waypoint markers -----
  useEffect(() => {
    const m = mapRef.current;
    if (!m) return;
    for (const mk of markers.current) mk.remove();
    markers.current = waypoints.map((p, i) => {
      const el = document.createElement("div");
      const isEnd = i === waypoints.length - 1 && i > 0;
      el.className = cx(
        "grid size-7 place-items-center rounded-full border-2 border-white text-[11px] font-bold text-white shadow-lg cursor-grab",
        i === 0 ? "bg-emerald-600" : isEnd ? "bg-rose-600" : "bg-slate-600",
      );
      el.textContent = i === 0 ? "A" : isEnd ? "B" : String(i);
      const mk = new maplibregl.Marker({ element: el, draggable: true }).setLngLat(p).addTo(m);
      mk.on("dragend", () => {
        const ll = mk.getLngLat();
        setWaypoints((w) => w.map((q, k) => (k === i ? [ll.lng, ll.lat] : q)));
      });
      return mk;
    });
  }, [waypoints]);

  // ----- route + profile -----
  useEffect(() => {
    if (waypoints.length < 2) {
      setRoute(null);
      setElev(null);
      setRouteError(undefined);
      return;
    }
    let live = true;
    setRouting(true);
    setRouteError(undefined);
    api
      .route({
        locations: waypoints.map(([lon, lat]) => ({ lon, lat })),
        profile: profileName,
        avoid_unpaved: avoidUnpaved,
      })
      .then(async (r) => {
        if (!live) return;
        setRoute(r);
        const coords = r.geometry.coordinates as LonLat[];
        fitRoute(coords);
        const len = lineLength(coords);
        const p = await api.profile(
          coords,
          Math.max(30, Math.round(len / 1500)),
          bridgeDeck ? "deck" : "ground",
        );
        if (live) setElev(p);
      })
      .catch((e: Error) => {
        if (!live) return;
        setRoute(null);
        setElev(null);
        setRouteError(e.message);
      })
      .finally(() => live && setRouting(false));
    return () => {
      live = false;
    };
  }, [waypoints, profileName, avoidUnpaved, bridgeDeck, fitRoute]);

  // ----- hover marker synced with the chart -----
  useEffect(() => {
    const m = mapRef.current;
    const s = hoverIdx != null ? elev?.samples[hoverIdx] : undefined;
    if (!m || !s) {
      hoverMarker.current?.remove();
      hoverMarker.current = null;
      return;
    }
    if (!hoverMarker.current) {
      const el = document.createElement("div");
      el.className = "size-3.5 rounded-full border-[3px] border-white bg-blue-600 shadow-md";
      hoverMarker.current = new maplibregl.Marker({ element: el });
    }
    hoverMarker.current.setLngLat([s.lon, s.lat]).addTo(m);
  }, [hoverIdx, elev]);

  // ----- point readout marker -----
  useEffect(() => {
    const m = mapRef.current;
    pointMarker.current?.remove();
    pointMarker.current = null;
    if (!m || !point) return;
    const el = document.createElement("div");
    el.className = "size-3 rounded-full border-2 border-white bg-amber-500 shadow-md";
    pointMarker.current = new maplibregl.Marker({ element: el }).setLngLat(point.at).addTo(m);
  }, [point]);

  const onMap = useCallback((m: MlMap | null) => {
    mapRef.current = m;
    if (!m) return;
    m.on("click", (e) => {
      const at: LonLat = [e.lngLat.lng, e.lngLat.lat];
      if (modeRef.current === "route") {
        setWaypoints((w) => [...w, at]);
        return;
      }
      setPoint({ at, loading: true });
      api
        .elevation([at])
        .then((r) => setPoint({ at, sample: r.results[0], loading: false }))
        .catch(() => setPoint({ at, loading: false }));
    });
    m.on("load", () => m.getCanvas().style.setProperty("cursor", ""));
  }, []);

  useEffect(() => {
    const c = mapRef.current?.getCanvas();
    if (c) c.style.cursor = mode === "route" ? "crosshair" : "";
  }, [mode]);

  const clearRoute = () => {
    setWaypoints([]);
    lastRouteFit.current = "";
    setHoverIdx(null);
  };

  const noData = gens.data && !hasLive;
  const thumbCenter: LonLat = installed.length
    ? [
        (installed[0].bbox[0] + installed[0].bbox[2]) / 2,
        (installed[0].bbox[1] + installed[0].bbox[3]) / 2,
      ]
    : [-100, 40];

  return (
    <div className="absolute inset-0">
      <MapCanvas style={style} onMap={onMap} onStyle={syncOverlays} />

      {/* Toolbar */}
      <div
        className={cx(
          "pointer-events-none absolute inset-x-3 top-3 flex flex-wrap items-start gap-2",
          noData && "hidden",
        )}
      >
        <div className="pointer-events-auto flex items-center gap-1 rounded-xl border border-line bg-panel/95 p-1 shadow-float backdrop-blur">
          <Segmented<Mode>
            label="Map tool"
            value={mode}
            onChange={(m) => {
              setMode(m);
              if (m === "route") setPoint(undefined);
            }}
            options={[
              {
                value: "inspect",
                label: (
                  <>
                    <MousePointerClick className="size-3.5" /> Inspect
                  </>
                ),
                title: "Click for elevation",
              },
              {
                value: "route",
                label: (
                  <>
                    <RouteIcon className="size-3.5" /> Route
                  </>
                ),
                title: "Click to add waypoints",
              },
            ]}
          />
        </div>
        {!noData && (
          <div className="pointer-events-auto rounded-xl border border-line bg-panel/95 p-1 shadow-float backdrop-blur">
            {styles.data?.length ? (
              <StylePicker
                styles={styles.data}
                value={styleName}
                onChange={setStyleChoice}
                center={thumbCenter}
              />
            ) : (
              <span className="flex h-8 items-center gap-1.5 px-2 text-[13px] text-faint">
                <Layers className="size-3.5" /> {styleName}
              </span>
            )}
          </div>
        )}
      </div>

      {/* First run / no generation */}
      {noData && (
        <div className="absolute inset-0 grid place-items-center p-4">
          <div className="w-full max-w-md rounded-2xl border border-line bg-panel p-6 text-center shadow-float">
            {active ? (
              <>
                <div className="mx-auto mb-3 grid size-11 place-items-center rounded-xl bg-accent-soft text-accent-strong">
                  <Loader2 className="size-5 animate-spin" />
                </div>
                <h2 className="font-semibold">Building your first map</h2>
                <p className="mt-1 text-sm text-muted">
                  {active.stages.find((s) => s.status === "running")?.name ?? "Queued"} ·{" "}
                  {fmt.percent(active.progress)}
                </p>
                <Progress value={active.progress} live className="mt-4" />
                <Link
                  to="/builds"
                  className="mt-3 inline-block text-sm text-accent hover:underline"
                >
                  Watch progress
                </Link>
              </>
            ) : (
              <>
                <div className="mx-auto mb-3 grid size-11 place-items-center rounded-xl bg-accent-soft text-accent-strong">
                  <MapPin className="size-5" />
                </div>
                <h2 className="font-semibold">No map data yet</h2>
                <p className="mx-auto mt-1 max-w-xs text-sm text-muted">
                  Pick the areas you need: a state, a province, or a box you draw. mapthing
                  downloads them and builds tiles, routing and elevation.
                </p>
                <Link to="/regions?add=1">
                  <Button variant="primary" className="mt-4" icon={<Plus className="size-4" />}>
                    Add a region
                  </Button>
                </Link>
              </>
            )}
          </div>
        </div>
      )}

      {/* Point readout */}
      {mode === "inspect" && point && (
        <div className="absolute right-3 bottom-8 left-3 max-w-xs rounded-xl border border-line bg-panel/95 p-3 shadow-float backdrop-blur sm:left-auto sm:w-72">
          <div className="flex items-start justify-between gap-2">
            <div className="flex items-center gap-2">
              <Mountain className="size-4 text-amber-500" />
              <span className="tnum text-lg font-semibold">
                {point.loading ? "…" : fmt.elevation(point.sample?.elevation_m)}
              </span>
            </div>
            <button
              type="button"
              aria-label="Close"
              onClick={() => setPoint(undefined)}
              className="rounded p-0.5 text-muted hover:text-fg"
            >
              <X className="size-4" />
            </button>
          </div>
          <div className="tnum mt-1 text-xs text-muted">{formatLonLat(point.at)}</div>
          {!point.loading &&
            (point.sample?.elevation_m != null ? (
              <div className="mt-1.5 text-xs text-muted">
                {sourceName(point.sample.source)}
                {point.sample.resolution_m ? ` · ${point.sample.resolution_m} m resolution` : ""}
              </div>
            ) : (
              <div className="mt-1.5 text-xs text-faint">
                No elevation data here (outside installed regions)
              </div>
            ))}
        </div>
      )}

      {/* Route panel */}
      {mode === "route" && (
        <div
          ref={routePanel}
          className="absolute inset-x-0 bottom-0 max-h-[60%] overflow-y-auto rounded-t-2xl border border-line bg-panel shadow-float sm:inset-x-auto sm:top-16 sm:bottom-auto sm:left-3 sm:max-h-[calc(100%-5rem)] sm:w-[380px] sm:rounded-2xl"
        >
          <div className="space-y-3 p-4">
            <div className="flex items-center justify-between">
              <h2 className="text-sm font-semibold">Route</h2>
              {waypoints.length > 0 && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={clearRoute}
                  icon={<Trash2 className="size-3.5" />}
                >
                  Clear
                </Button>
              )}
            </div>
            <div className="flex flex-wrap items-center gap-3">
              <Segmented<RouteProfile>
                label="Vehicle"
                size="sm"
                value={profileName}
                onChange={setProfileName}
                options={[
                  { value: "auto", label: "Car" },
                  {
                    value: "truck",
                    label: (
                      <>
                        <Truck className="size-3.5" /> Truck
                      </>
                    ),
                  },
                ]}
              />
              <div className="flex items-center gap-2 text-xs text-muted">
                <Switch
                  checked={avoidUnpaved}
                  onChange={setAvoidUnpaved}
                  label="Avoid unpaved roads"
                />
                Avoid unpaved
              </div>
            </div>

            {waypoints.length === 0 ? (
              <p className="rounded-lg bg-sunken px-3 py-2.5 text-xs text-muted">
                Click the map to set the start (A), then the destination (B). More clicks add stops.
                Drag markers to adjust.
              </p>
            ) : (
              <ol className="space-y-1">
                {waypoints.map((p, i) => (
                  <li key={i} className="flex items-center gap-2 text-xs">
                    <span
                      className={cx(
                        "grid size-5 shrink-0 place-items-center rounded-full text-[10px] font-bold text-white",
                        i === 0
                          ? "bg-emerald-600"
                          : i === waypoints.length - 1
                            ? "bg-rose-600"
                            : "bg-slate-600",
                      )}
                    >
                      {i === 0 ? "A" : i === waypoints.length - 1 ? "B" : i}
                    </span>
                    <span className="tnum flex-1 truncate text-muted">{formatLonLat(p)}</span>
                    <button
                      type="button"
                      aria-label={`Remove waypoint ${i + 1}`}
                      onClick={() => setWaypoints((w) => w.filter((_, k) => k !== i))}
                      className="rounded p-0.5 text-faint hover:text-bad"
                    >
                      <X className="size-3.5" />
                    </button>
                  </li>
                ))}
                {waypoints.length === 1 && (
                  <li className="pl-7 text-xs text-faint">Click the map for B…</li>
                )}
              </ol>
            )}

            {routing && (
              <div className="flex items-center gap-2 text-xs text-muted">
                <Loader2 className="size-3.5 animate-spin" /> Routing…
              </div>
            )}
            {routeError && (
              <p className="rounded-lg bg-bad/10 px-3 py-2 text-xs text-bad">{routeError}</p>
            )}

            {route && (
              <>
                <div className="grid grid-cols-3 gap-3 border-t border-line pt-3">
                  <div>
                    <div className="text-[11px] text-faint">Distance</div>
                    <div className="tnum text-base font-semibold">
                      {fmt.distance(route.distance_m)}
                    </div>
                  </div>
                  <div>
                    <div className="text-[11px] text-faint">Duration</div>
                    <div className="tnum text-base font-semibold">
                      {fmt.duration(route.duration_s)}
                    </div>
                  </div>
                  <div>
                    <div className="text-[11px] text-faint">Stops</div>
                    <div className="tnum text-base font-semibold">
                      {Math.max(0, waypoints.length - 2)}
                    </div>
                  </div>
                </div>
                <div>
                  <div className="mb-1.5 text-[11px] font-medium text-faint">Surface</div>
                  <PartsBar
                    parts={SURFACES.map((s) => ({
                      ...s,
                      value: route.surface_breakdown_m[s.key] ?? 0,
                    }))}
                  />
                </div>
              </>
            )}

            {elev && (
              <div className="border-t border-line pt-3">
                <div className="mb-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
                  <span className="inline-flex items-center gap-1 text-muted">
                    <ArrowUpRight className="size-3.5 text-ok" />
                    <span className="tnum font-medium text-fg">{fmt.elevation(elev.gain_m)}</span>
                  </span>
                  <span className="inline-flex items-center gap-1 text-muted">
                    <ArrowDownRight className="size-3.5 text-bad" />
                    <span className="tnum font-medium text-fg">{fmt.elevation(elev.loss_m)}</span>
                  </span>
                  <span className="text-muted">
                    min <span className="tnum text-fg">{fmt.elevation(elev.min_m)}</span>
                  </span>
                  <span className="text-muted">
                    max <span className="tnum text-fg">{fmt.elevation(elev.max_m)}</span>
                  </span>
                </div>
                <div className="mb-2 flex flex-wrap items-center gap-2 text-xs text-muted">
                  <Switch
                    checked={bridgeDeck}
                    onChange={setBridgeDeck}
                    label="Follow the road over bridges and through tunnels"
                  />
                  <span title="On: the profile runs straight across bridges and tunnels at road level. Off: it follows the ground under them, as a buried pipe would.">
                    Road level on bridges
                  </span>
                  {elev.bridges === "deck" && (elev.structure_spans?.length ?? 0) > 0 && (
                    <span className="text-faint">({elev.structure_spans!.length} crossed)</span>
                  )}
                </div>
                {elev.warning && (
                  <p className="mb-2 rounded-lg bg-warn/10 px-3 py-2 text-xs text-warn">
                    {elev.warning}
                  </p>
                )}
                <ElevationChart
                  samples={elev.samples}
                  hoverIndex={hoverIdx}
                  onHover={setHoverIdx}
                />
                <div className="mt-1 text-[11px] text-faint">
                  Elevation: {elev.sources.map(sourceName).join(", ")}
                </div>
              </div>
            )}

            {route?.legs.some((l) => l.maneuvers?.length) && (
              <details className="border-t border-line pt-3 text-xs">
                <summary className="cursor-pointer font-medium text-muted">Directions</summary>
                <ol className="mt-2 space-y-1.5">
                  {route.legs.flatMap((l, li) =>
                    (l.maneuvers ?? []).map((mv, mi) => (
                      <li key={`${li}-${mi}`} className="flex justify-between gap-3">
                        <span>{mv.instruction}</span>
                        <span className="tnum shrink-0 text-faint">
                          {mv.distance_m ? fmt.distance(mv.distance_m) : ""}
                        </span>
                      </li>
                    )),
                  )}
                </ol>
              </details>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
