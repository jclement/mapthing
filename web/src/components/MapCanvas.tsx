import type { Map as MlMap, StyleSpecification } from "maplibre-gl";
import * as maplibregl from "maplibre-gl";
import workerUrl from "maplibre-gl/dist/maplibre-gl-worker.mjs?url";
import { useEffect, useRef } from "react";

// MapLibre locates its worker relative to its own module, which breaks once bundled.
// Point it at the (self-contained) worker file Vite emits as an asset.
maplibregl.setWorkerUrl(workerUrl);

import type { BBox } from "../api";

/** A style with no data: used before the first generation exists. */
export function blankStyle(dark: boolean): StyleSpecification {
  return {
    version: 8,
    sources: {},
    layers: [
      {
        id: "background",
        type: "background",
        paint: { "background-color": dark ? "#1b1f27" : "#eef0f2" },
      },
    ],
  };
}

/**
 * A MapLibre map that fills its parent. `onStyle` runs after every style load (initial and
 * after a style switch) so callers can (re)add their own sources and layers.
 */
export function MapCanvas({
  style,
  onMap,
  onStyle,
  initialBounds,
  className,
}: {
  style: string | StyleSpecification;
  onMap?: (map: MlMap | null) => void;
  onStyle?: (map: MlMap) => void;
  initialBounds?: BBox;
  className?: string;
}) {
  const el = useRef<HTMLDivElement>(null);
  const map = useRef<MlMap | null>(null);
  const onStyleRef = useRef(onStyle);
  onStyleRef.current = onStyle;
  const onMapRef = useRef(onMap);
  onMapRef.current = onMap;
  const boundsRef = useRef(initialBounds);
  const styleRef = useRef(style);

  useEffect(() => {
    if (!el.current) return;
    let m: MlMap;
    try {
      m = new maplibregl.Map({
        container: el.current,
        style: styleRef.current,
        center: [-100, 40],
        zoom: 3,
        attributionControl: { compact: true },
        ...(boundsRef.current
          ? {
              bounds: boundsRef.current as [number, number, number, number],
              fitBoundsOptions: { padding: 40 },
            }
          : {}),
      });
    } catch (err) {
      // No WebGL (old browser, headless without GPU): leave the container empty.
      console.warn("map unavailable", err);
      return;
    }
    m.addControl(new maplibregl.NavigationControl({ visualizePitch: true }), "top-right");
    m.addControl(new maplibregl.ScaleControl({ unit: "metric" }), "bottom-left");
    m.on("style.load", () => onStyleRef.current?.(m));
    m.on("error", (e) => console.warn("map:", e.error?.message ?? e));
    map.current = m;
    if (import.meta.env.VITE_MOCK) (window as unknown as { __map: MlMap }).__map = m;
    onMapRef.current?.(m);
    return () => {
      onMapRef.current?.(null);
      m.remove();
      map.current = null;
    };
  }, []);

  useEffect(() => {
    if (styleRef.current === style) return;
    styleRef.current = style;
    map.current?.setStyle(style, { diff: false });
  }, [style]);

  // maplibre-gl.css sets `.maplibregl-map { position: relative }` outside any cascade layer,
  // which beats Tailwind utilities on the same element; so position a wrapper instead.
  return (
    <div className={className ?? "absolute inset-0"}>
      <div ref={el} className="h-full w-full" />
    </div>
  );
}
