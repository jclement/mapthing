#!/usr/bin/env python3
"""Turn the sizing measurements into per-country estimates.

    scripts/sizing-model.py [--json out.json]

Inputs (all under docs/sizing/):
  measurements.csv     region builds on the reference machine (scripts/measure-build.sh)
  terrain.csv          terrain builds of 1x1 degree boxes (scripts/measure-terrain.sh)
  planet-extracts.csv  basemap sizes cut from the published Protomaps planet build
                       (`pmtiles extract --dry-run`), validated against our own builds
  packs.csv, raster.csv, inputs.json (published facts: PBF sizes, areas, DEM volumes)

Every figure carries a basis:
  measured     built and measured on the reference machine
  published    taken or cut from published data (a HEAD, an S3 listing, the planet build)
  extrapolated a ratio or fit from measured builds applied outside the measured range
Ranges are the spread of the measured ratios (min..max), so they say how much regions differ,
not a statistical interval. Nothing here is a promise for a different machine.
"""
import csv, json, math, os, statistics, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
D = os.path.join(ROOT, "docs", "sizing")
GB, MB = 1e9, 1e6
TERRAIN_BUDGET = 450_000  # internal/build/terrain.go DefaultTerrainMaxTiles


def rows(name):
    p = os.path.join(D, name)
    return list(csv.DictReader(open(p))) if os.path.exists(p) else []


def f(r, k):
    try:
        return float(r[k])
    except (KeyError, ValueError, TypeError):
        return 0.0


def rng(vals):
    vals = [v for v in vals if v > 0]
    return {"low": min(vals), "mid": statistics.median(vals), "high": max(vals)}


def scale(r, x):
    return {k: v * x for k, v in r.items()}


def add(*rs):
    return {k: sum(r[k] for r in rs) for k in ("low", "mid", "high")}


def linfit(xs, ys):
    n = len(xs)
    mx, my = sum(xs) / n, sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    k = sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / sxx
    return my - k * mx, k


def main():
    out_json = sys.argv[sys.argv.index("--json") + 1] if "--json" in sys.argv else None
    inputs = json.load(open(os.path.join(D, "inputs.json")))
    meas = rows("measurements.csv")
    terr = rows("terrain.csv")
    planet = rows("planet-extracts.csv")

    # Latest clean run per region (sources already cached, not the first run).
    runs = {}
    for r in meas:
        if r["sources_cached"] == "1" and "first run" not in r["note"]:
            runs[r["label"]] = r
    # Ratios use regions above 100 MB of PBF: below that, fixed per-build costs dominate.
    runs = [r for r in runs.values() if f(r, "pbf_bytes") > 1e8]
    pbf = [f(r, "pbf_bytes") for r in runs]

    # --- ratios and rates from measured builds
    m = {}
    m["dl_Bps"] = rng([f(r, "pbf_bytes") / f(r, "t_download_s") for r in runs if f(r, "t_download_s") > 5])
    m["valhalla_tar_per_pbf"] = rng([f(r, "valhalla_tar_bytes") / f(r, "pbf_bytes") for r in runs])
    m["valhalla_s_per_gb"] = rng([f(r, "t_valhalla_s") / (f(r, "pbf_bytes") / GB) for r in runs if f(r, "pbf_bytes") > 1e8])
    c, k = linfit(pbf, [f(r, "t_planetiler_s") for r in runs])
    m["planetiler_fit"] = {"fixed_s": c, "s_per_gb": k * GB}
    m["planetiler_s_per_gb"] = rng([(f(r, "t_planetiler_s") - c) / (f(r, "pbf_bytes") / GB) for r in runs if f(r, "pbf_bytes") > 1e8])
    # Scratch beyond inputs and outputs: Planetiler temp files and Valhalla's loose tiles.
    m["scratch_per_pbf"] = rng([
        (f(r, "peak_disk_above_base_mb") * 1048576 - f(r, "pbf_bytes") - f(r, "basemap_bytes") - f(r, "valhalla_tar_bytes"))
        / f(r, "pbf_bytes") for r in runs if f(r, "pbf_bytes") > 1e8])
    m["anon_peak_mb"] = rng([f(r, "cgroup_anon_peak_mb") for r in runs])

    # --- terrain per max-zoom tile, by source family (1-degree boxes; cold cache rows)
    fam = {"us": ("permian", "haynesville"), "canada": ("alberta",), "saudi": ("saudi",)}
    tstats = {}
    for key, needle in fam.items():
        sel = [t for t in terr if any(n in t["label"] for n in needle) and "deg" in t["label"] and "warm" not in t["note"]]
        if not sel:
            continue
        tstats[key] = {
            "bytes_per_tile": rng([f(t, "archive_bytes") / f(t, "maxzoom_tiles") for t in sel]),
            "s_per_tile": rng([f(t, "t_s") / f(t, "maxzoom_tiles") for t in sel]),
            "fetch_per_tile": rng([f(t, "cache_bytes") / f(t, "maxzoom_tiles") for t in sel]),
            "peak_rss_mb": rng([f(t, "peak_rss_mb") for t in sel]),
            "boxes": [t["label"] for t in sel],
        }
    warm = [t for t in terr if "warm" in t["note"]]
    cold = {t["label"]: t for t in terr if "warm" not in t["note"]}
    warm_ratio = None
    if warm:
        w = warm[-1]
        cl = cold.get(w["label"])
        if cl:
            warm_ratio = f(w, "t_s") / f(cl, "t_s")

    pe = {(r["region"], int(r["maxzoom"])): f(r, "bytes") for r in planet}
    pe_key = {"us": "us", "canada": "canada", "saudi": "saudi"}

    out = {"basis_legend": {"measured": "built on the reference machine", "published": "from published data",
                            "extrapolated": "measured ratio applied outside the measured range"},
           "measured_ratios": m, "terrain_per_tile": tstats, "terrain_warm_over_cold": warm_ratio,
           "countries": {}}

    for cid, cinfo in inputs["countries"].items():
        p = float(cinfo["pbf_bytes"])
        e = {"name": cinfo["name"], "pbf_bytes": p}
        e["download_s"] = scale({k: 1 / v for k, v in m["dl_Bps"].items()}, p)
        e["download_s"] = {"low": e["download_s"]["high"], "mid": e["download_s"]["mid"], "high": e["download_s"]["low"]}
        e["basemap_bytes_z15"] = pe.get((pe_key[cid], 15))
        e["basemap_bytes_z14"] = pe.get((pe_key[cid], 14))
        e["planetiler_s"] = {k: c + v * p / GB for k, v in m["planetiler_s_per_gb"].items()}
        e["valhalla_tar_bytes"] = scale(m["valhalla_tar_per_pbf"], p)
        e["valhalla_s"] = scale(m["valhalla_s_per_gb"], p / GB)
        # Terrain as the build would choose it today: native zoom, then the tile budget over the
        # region's polygon (Geofabrik polygons include a coastal buffer).
        native = 12 if cid == "saudi" else 13
        z10 = cinfo["z10_tiles"]
        z = native
        while z10 * 4 ** (z - 10) > TERRAIN_BUDGET and z > 8:
            z -= 1
        today_tiles = z10 * 4 ** (z - 10)
        full_z = 12 if cid in ("canada", "saudi") else 13
        full_tiles = cinfo["terrain_land_z10_tiles_est"] * 4 ** (full_z - 10)
        ts = tstats.get(cid)
        if ts:
            for tag, zz, n in (("today", z, today_tiles), ("full", full_z, full_tiles)):
                e[f"terrain_{tag}"] = {
                    "maxzoom": zz, "tiles": n,
                    "bytes": scale(ts["bytes_per_tile"], n),
                    "build_s_cold": scale(ts["s_per_tile"], n),
                    "fetch_bytes": scale(ts["fetch_per_tile"], n),
                }
        # Disk: steady state keeps the download, two generations and the caches.
        bm = e["basemap_bytes_z15"] or 0
        tt = e.get("terrain_today", {}).get("bytes", {"low": 0, "mid": 0, "high": 0})
        gen = add({"low": bm, "mid": bm, "high": bm}, e["valhalla_tar_bytes"], tt)
        e["generation_bytes"] = gen
        e["steady_disk_bytes"] = add({"low": p, "mid": p, "high": p}, scale(gen, 2),
                                     e.get("terrain_today", {}).get("fetch_bytes", {"low": 0, "mid": 0, "high": 0}))
        scratch = scale(m["scratch_per_pbf"], p)
        e["build_peak_extra_bytes"] = add(gen, scratch)  # new generation + temp files, on top of steady
        out["countries"][cid] = e

    # --- chart-friendly summary (mid values, GB and hours) plus the raw tables
    wr = warm_ratio or 1.0
    summary = {}
    for cid, e in out["countries"].items():
        tt = e.get("terrain_today", {})
        t_terr = tt.get("build_s_cold", {}).get("mid", 0)
        build_h = (e["download_s"]["mid"] + e["planetiler_s"]["mid"] + e["valhalla_s"]["mid"] + t_terr) / 3600
        refresh_h = (e["download_s"]["mid"] + e["planetiler_s"]["mid"] + e["valhalla_s"]["mid"] + t_terr * wr) / 3600
        summary[cid] = {
            "name": e["name"],
            "pbf_gb": e["pbf_bytes"] / GB,
            "storage_gb": {"basemap_z15": (e["basemap_bytes_z15"] or 0) / GB,
                           "valhalla": e["valhalla_tar_bytes"]["mid"] / GB,
                           "terrain_today": tt.get("bytes", {}).get("mid", 0) / GB},
            "terrain_full_gb": e.get("terrain_full", {}).get("bytes", {}).get("mid", 0) / GB,
            "steady_disk_gb": e["steady_disk_bytes"]["mid"] / GB,
            "peak_disk_gb": (e["steady_disk_bytes"]["mid"] + e["build_peak_extra_bytes"]["mid"]) / GB,
            "time_h": {"download": e["download_s"]["mid"] / 3600, "basemap": e["planetiler_s"]["mid"] / 3600,
                       "routing": e["valhalla_s"]["mid"] / 3600, "terrain_cold": t_terr / 3600},
            "full_build_h": build_h, "refresh_h": refresh_h,
        }
    out["summary"] = summary
    out["packs"] = rows("packs.csv")
    out["planet_extracts"] = planet
    out["raster"] = rows("raster.csv")

    # --- print
    def g(v):
        return f"{v/GB:,.1f} GB"

    def r_(d, fn):
        return f"{fn(d['mid'])} ({fn(d['low'])}–{fn(d['high'])})"

    def mins(v):
        return f"{v/60:,.0f} min"

    print("measured ratios:")
    for k, v in m.items():
        print(f"  {k}: {v}")
    print("terrain per max-zoom tile:")
    for k, v in tstats.items():
        print(f"  {k}: bytes {r_(v['bytes_per_tile'], lambda x: f'{x/1e3:.0f} KB')}  s {r_(v['s_per_tile'], lambda x: f'{x:.3f}')}  "
              f"fetch {r_(v['fetch_per_tile'], lambda x: f'{x/1e3:.0f} KB')}  rss {r_(v['peak_rss_mb'], lambda x: f'{x:.0f} MB')}")
    print(f"  warm/cold terrain time: {warm_ratio}")
    for cid, e in out["countries"].items():
        print(f"\n== {e['name']}  PBF {g(e['pbf_bytes'])}")
        print(f"  download        {r_(e['download_s'], mins)}")
        if e["basemap_bytes_z15"]:
            print(f"  basemap z15     {g(e['basemap_bytes_z15'])} (published planet cut); z14 {g(e['basemap_bytes_z14'])}")
        print(f"  planetiler      {r_(e['planetiler_s'], mins)}")
        print(f"  valhalla tar    {r_(e['valhalla_tar_bytes'], g)}   time {r_(e['valhalla_s'], mins)}")
        for tag in ("today", "full"):
            t = e.get(f"terrain_{tag}")
            if t:
                print(f"  terrain {tag:5s}   z{t['maxzoom']} {t['tiles']:,} tiles  {r_(t['bytes'], g)}  build {r_(t['build_s_cold'], mins)}  fetch {r_(t['fetch_bytes'], g)}")
        print(f"  generation      {r_(e['generation_bytes'], g)}")
        print(f"  steady disk     {r_(e['steady_disk_bytes'], g)}  (+ build peak extra {r_(e['build_peak_extra_bytes'], g)})")
    if out_json:
        os.makedirs(os.path.dirname(out_json), exist_ok=True)
        json.dump(out, open(out_json, "w"), indent=2)
        print(f"\nwrote {out_json}")


if __name__ == "__main__":
    main()
