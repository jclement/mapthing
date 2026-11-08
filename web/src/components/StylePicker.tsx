import { Check, ChevronDown, Layers } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { type LonLat, type StyleInfo, type StyleName, styleThumbUrl } from "../api";
import { cx } from "./ui";

/** Slippy-map tile containing a point. */
export function tileAt([lon, lat]: LonLat, z: number): [number, number] {
  const n = 2 ** z;
  const x = Math.floor(((lon + 180) / 360) * n);
  const r = (Math.max(-85, Math.min(85, lat)) * Math.PI) / 180;
  const y = Math.floor(((1 - Math.log(Math.tan(r) + 1 / Math.cos(r)) / Math.PI) / 2) * n);
  return [Math.min(n - 1, Math.max(0, x)), Math.min(n - 1, Math.max(0, y))];
}

function Swatch({ s }: { s: StyleInfo }) {
  const bg = s.id.startsWith("overlay")
    ? "repeating-conic-gradient(#cbd5e1 0 25%, #f1f5f9 0 50%) 0 0 / 10px 10px"
    : s.id === "satellite"
      ? "linear-gradient(135deg,#3f4f2c,#6b6a3a 45%,#2f3b2a)"
      : s.dark
        ? "linear-gradient(135deg,#1f2530,#2b3442)"
        : s.id === "field"
          ? "linear-gradient(135deg,#f6f1e4,#e6d9b8)"
          : "linear-gradient(135deg,#eef3e8,#dfe9d6)";
  return <span className="absolute inset-0" style={{ background: bg }} />;
}

function Thumb({ s, center }: { s: StyleInfo; center: LonLat }) {
  const [failed, setFailed] = useState(false);
  const z = 11;
  const [x, y] = tileAt(center, z);
  // One 256 px tile from the raster renderer; falls back to a swatch (mock mode, no data).
  const src = import.meta.env.VITE_MOCK || failed ? null : styleThumbUrl(s.id, z, x, y);
  return (
    <span className="relative block size-12 shrink-0 overflow-hidden rounded-md ring-1 ring-line">
      <Swatch s={s} />
      {src && (
        <img
          src={src}
          alt=""
          loading="lazy"
          onError={() => setFailed(true)}
          className="absolute inset-0 size-full object-cover"
        />
      )}
    </span>
  );
}

/** Data-driven style switcher (styles come from GET /styles). */
export function StylePicker({
  styles,
  value,
  onChange,
  center,
}: {
  styles: StyleInfo[];
  value: StyleName;
  onChange: (id: StyleName) => void;
  center: LonLat;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const current = styles.find((s) => s.id === value);
  useEffect(() => {
    if (!open) return;
    const fn = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false);
    };
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", fn);
    window.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("mousedown", fn);
      window.removeEventListener("keydown", key);
    };
  }, [open]);

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label="Map style"
        onClick={() => setOpen((o) => !o)}
        className="flex h-8 items-center gap-1.5 rounded-lg px-2 text-[13px] font-medium hover:bg-sunken"
      >
        <Layers className="size-3.5 text-faint" />
        {current?.name ?? value}
        <ChevronDown
          className={cx("size-3.5 text-faint transition-transform", open && "rotate-180")}
        />
      </button>
      {open && (
        <div
          role="listbox"
          aria-label="Map styles"
          className="absolute top-full left-0 z-20 mt-2 w-72 max-w-[calc(100vw-1.5rem)] space-y-0.5 rounded-xl border border-line bg-panel p-1.5 shadow-float"
        >
          {styles.map((s) => (
            <div key={s.id}>
              <button
                type="button"
                role="option"
                aria-selected={s.id === value}
                onClick={() => {
                  onChange(s.id);
                  setOpen(false);
                }}
                className={cx(
                  "flex w-full items-center gap-3 rounded-lg p-1.5 text-left",
                  s.id === value ? "bg-accent-soft" : "hover:bg-sunken",
                )}
              >
                <Thumb s={s} center={center} />
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-1.5 text-[13px] font-medium">
                    {s.name}
                    {s.id === value && <Check className="size-3.5 text-accent" />}
                  </span>
                  <span className="line-clamp-2 text-xs text-muted">{s.description}</span>
                </span>
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
