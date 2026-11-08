import type { Map as MlMap } from "maplibre-gl";
import { useCallback, useEffect, useRef } from "react";
import { TerraDraw, TerraDrawRectangleMode } from "terra-draw";
import { TerraDrawMapLibreGLAdapter } from "terra-draw-maplibre-gl-adapter";
import { type BBox, type StyleName, styleUrl } from "../api";
import { bboxOf } from "../lib/geo";
import { useTheme } from "../lib/theme";
import { MapCanvas } from "./MapCanvas";

/** A map on which the user drags out one rectangle. Reports its bbox. */
export function DrawBox({
  onChange,
  initialBounds,
  hasData,
}: {
  onChange: (b: BBox | null) => void;
  initialBounds?: BBox;
  hasData: boolean;
}) {
  const { dark } = useTheme();
  const draw = useRef<TerraDraw | null>(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const name: StyleName = dark ? "outdoors-dark" : "outdoors";

  const start = useCallback((m: MlMap) => {
    if (draw.current) return;
    const td = new TerraDraw({
      adapter: new TerraDrawMapLibreGLAdapter({ map: m }),
      modes: [
        new TerraDrawRectangleMode({
          styles: {
            fillColor: "#0d9488",
            fillOpacity: 0.15,
            outlineColor: "#0d9488",
            outlineWidth: 2,
          },
        }),
      ],
    });
    td.start();
    td.setMode("rectangle");
    td.on("finish", (id) => {
      const snap = td.getSnapshot();
      for (const f of snap) if (f.id !== id) td.removeFeatures([f.id as string]);
      const f = snap.find((x) => x.id === id);
      if (f && f.geometry.type === "Polygon") {
        onChangeRef.current(bboxOf(f.geometry.coordinates[0] as [number, number][]));
      }
    });
    draw.current = td;
  }, []);

  useEffect(
    () => () => {
      try {
        draw.current?.stop();
      } catch {
        /* map already removed */
      }
      draw.current = null;
    },
    [],
  );

  return (
    <div className="relative h-full min-h-72 overflow-hidden rounded-xl border border-line">
      <MapCanvas
        // Without a generation there is no local basemap; MapLibre's demo tiles give
        // enough context to place a box.
        style={hasData ? styleUrl(name) : "https://demotiles.maplibre.org/style.json"}
        initialBounds={initialBounds}
        onMap={(m) => {
          if (!m) return;
          if (m.loaded()) start(m);
          else m.once("load", () => start(m));
        }}
      />
      <div className="pointer-events-none absolute top-2 left-2 rounded-lg bg-panel/90 px-2.5 py-1.5 text-xs text-muted shadow">
        Click once to start the box, click again to finish
      </div>
    </div>
  );
}
