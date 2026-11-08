#!/usr/bin/env python3
"""Fit simple build-cost models to measured region builds and extrapolate to whole countries.

    scripts/sizing-model.py [--json out.json]

Reads docs/sizing/measurements.csv (from scripts/measure-build.sh), docs/sizing/terrain.csv
(from scripts/measure-terrain.sh, optional), docs/sizing/inputs.json (published facts), and
prints the fitted models plus a per-country table. Every number carries a basis:
  measured      a build on the reference machine
  extrapolated  a fit to measured builds, evaluated outside the measured range
  published     taken from a source's published data (sizes, benchmarks)

Fits (least squares, stdlib only):
  sizes  ~ a * pbf^b            (log-log; output size grows a little sub/super-linearly)
  times  ~ c + k * pbf          (a fixed per-build overhead plus a per-byte cost)
Error bands are the min/max ratio measured/fitted across the measured regions, widened by
50% beyond the largest measured input (extrapolation risk), never narrower than +/-20%.
"""
import csv, json, math, os, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GB = 1e9


def load_csv(path):
    if not os.path.exists(path):
        return []
    with open(path) as f:
        return list(csv.DictReader(f))


def powfit(xs, ys):
    lx = [math.log(x) for x in xs]
    ly = [math.log(y) for y in ys]
    n = len(xs)
    mx, my = sum(lx) / n, sum(ly) / n
    sxx = sum((a - mx) ** 2 for a in lx)
    b = sum((a - mx) * (c - my) for a, c in zip(lx, ly)) / sxx if sxx else 1.0
    a = math.exp(my - b * mx)
    return lambda x: a * x ** b, {"form": "a*pbf^b", "a": a, "b": b}


def linfit(xs, ys):
    n = len(xs)
    mx, my = sum(xs) / n, sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    k = sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / sxx if sxx else 0.0
    c = max(0.0, my - k * mx)
    return lambda x: c + k * x, {"form": "c+k*pbf", "c": c, "k_per_gb": k * GB}


def band(f, xs, ys, x):
    rs = [y / f(xx) for xx, y in zip(xs, ys) if f(xx) > 0]
    lo, hi = min(rs), max(rs)
    if x > max(xs) * 1.5:
        lo, hi = lo / 1.5, hi * 1.5
    lo, hi = min(lo, 0.8), max(hi, 1.2)
    return lo, hi


def main():
    out_json = None
    if "--json" in sys.argv:
        out_json = sys.argv[sys.argv.index("--json") + 1]
    rows = load_csv(os.path.join(ROOT, "docs/sizing/measurements.csv"))
    terr = load_csv(os.path.join(ROOT, "docs/sizing/terrain.csv"))
    packs = load_csv(os.path.join(ROOT, "docs/sizing/packs.csv"))
    inputs = json.load(open(os.path.join(ROOT, "docs/sizing/inputs.json")))

    # One row per label: the latest run with sources already cached (clean timing).
    by = {}
    for r in rows:
        if r["sources_cached"] != "1" or "first run" in r["note"]:
            continue
        by[r["label"]] = r
    runs = list(by.values())
    if len(runs) < 2:
        sys.exit("need at least two measured regions")
    x = [float(r["pbf_bytes"]) for r in runs]

    def col(name, scale=1.0):
        return [float(r[name]) * scale for r in runs]

    models = {}
    specs = {
        "basemap_bytes": ("basemap_bytes", powfit, 1.0),
        "valhalla_tar_bytes": ("valhalla_tar_bytes", powfit, 1.0),
        "peak_scratch_bytes": ("peak_disk_above_base_mb", powfit, 1048576.0),
        "t_basemap_s": ("t_basemap_s", linfit, 1.0),
        "t_valhalla_s": ("t_valhalla_s", linfit, 1.0),
        "t_verify_s": ("t_verify_s", linfit, 1.0),
        "rss_valhalla_bytes": ("rss_valhalla_mb", powfit, 1048576.0),
    }
    for key, (c, fit, scale) in specs.items():
        ys = col(c, scale)
        pairs = [(a, b) for a, b in zip(x, ys) if b > 0]
        if len(pairs) < 2:
            continue
        xs_, ys_ = zip(*pairs)
        f, params = fit(list(xs_), list(ys_))
        models[key] = (f, params, list(xs_), list(ys_))

    # Download throughput: measured from the download stage of the larger extracts.
    dl = [(float(r["pbf_bytes"]), float(r["t_download_s"])) for r in runs if float(r["t_download_s"]) > 5]
    dl_bps = sum(b for b, _ in dl) / sum(t for _, t in dl) if dl else None

    print("== measured regions (clean runs)")
    for r in sorted(runs, key=lambda r: float(r["pbf_bytes"])):
        print(f"  {r['label']:14s} pbf {float(r['pbf_bytes'])/1e6:8.1f} MB  basemap {float(r['basemap_bytes'])/1e6:8.1f} MB  "
              f"valhalla {float(r['valhalla_tar_bytes'])/1e6:8.1f} MB  t_basemap {float(r['t_basemap_s']):7.1f}s  "
              f"t_valhalla {float(r['t_valhalla_s']):7.1f}s  peak+ {float(r['peak_disk_above_base_mb']):7.0f} MB  "
              f"rss_v {r['rss_valhalla_mb']} MB  cg {r['cgroup_mem_peak_mb']} MB")
    print("== fits")
    for k, (_, p, xs_, ys_) in models.items():
        print(f"  {k:20s} {p}")
    if dl_bps:
        print(f"  download throughput {dl_bps/1e6:.1f} MB/s (Geofabrik, this network)")

    # Terrain: bytes and seconds per max-zoom tile, by source.
    tstats = {}
    for t in terr:
        s = t["source"]
        tiles = float(t["maxzoom_tiles"]) or 1
        d = tstats.setdefault(s, {"bytes_per_tile": [], "s_per_tile": [], "fetch_per_tile": [], "rss_mb": []})
        d["bytes_per_tile"].append(float(t["archive_bytes"]) / tiles)
        d["s_per_tile"].append(float(t["t_s"]) / tiles)
        d["fetch_per_tile"].append(float(t["cache_bytes"]) / tiles)
        d["rss_mb"].append(float(t["peak_rss_mb"]))
    if tstats:
        print("== terrain per max-zoom tile (includes lower zooms)")
        for s, d in tstats.items():
            print(f"  {s:8s} bytes/tile {min(d['bytes_per_tile'])/1e3:.1f}-{max(d['bytes_per_tile'])/1e3:.1f} KB  "
                  f"s/tile {min(d['s_per_tile'])*1e3:.1f}-{max(d['s_per_tile'])*1e3:.1f} ms  "
                  f"fetched/tile {min(d['fetch_per_tile'])/1e3:.1f}-{max(d['fetch_per_tile'])/1e3:.1f} KB")

    maxx = max(x)
    result = {"generated_from": "docs/sizing/measurements.csv + inputs.json", "models": {k: v[1] for k, v in models.items()},
              "download_MBps": dl_bps / 1e6 if dl_bps else None, "countries": {}}
    print("== countries")
    for cid, c in inputs["countries"].items():
        pbf = float(c["pbf_bytes"])
        basis = "measured-range" if pbf <= maxx else "extrapolated"
        est = {"pbf_bytes": pbf, "basis": basis}
        for k, (f, p, xs_, ys_) in models.items():
            v = f(pbf)
            lo, hi = band(f, xs_, ys_, pbf)
            est[k] = {"value": v, "low": v * lo, "high": v * hi}
        if dl_bps:
            est["t_download_s"] = {"value": pbf / dl_bps}
        result["countries"][cid] = est
        def fmt(k, unit):
            e = est.get(k)
            if not e:
                return "n/a"
            if unit == "GB":
                return f"{e['value']/GB:.1f} GB ({e['low']/GB:.1f}-{e['high']/GB:.1f})"
            return f"{e['value']/60:.0f} min ({e['low']/60:.0f}-{e['high']/60:.0f})"
        print(f"  {c['name']} [{basis}] pbf {pbf/GB:.2f} GB")
        print(f"     basemap {fmt('basemap_bytes','GB')}  valhalla {fmt('valhalla_tar_bytes','GB')}  scratch {fmt('peak_scratch_bytes','GB')}")
        print(f"     t_basemap {fmt('t_basemap_s','min')}  t_valhalla {fmt('t_valhalla_s','min')}  rss_valhalla {fmt('rss_valhalla_bytes','GB')}")

    result["packs"] = packs
    result["terrain"] = {s: {k: [min(v), max(v)] for k, v in d.items()} for s, d in tstats.items()}
    if out_json:
        os.makedirs(os.path.dirname(out_json), exist_ok=True)
        json.dump(result, open(out_json, "w"), indent=2, default=float)
        print(f"wrote {out_json}")


if __name__ == "__main__":
    main()
