package build

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/jclement/mapthing/internal/catalog"
)

// unknownPBFBytes is assumed for an extract whose size is not known (no HEAD, not on disk).
const unknownPBFBytes = 512 << 20

// workFactor is the scratch and output a build needs per byte of merged OSM input: Planetiler's
// temporary store and the basemap, Valhalla's loose tiles and tar.
const workFactor = 4

// DiskEstimate is what a build is expected to need on the data volume, itemised.
type DiskEstimate struct {
	DownloadBytes int64    `json:"download_bytes"` // extracts to fetch (missing, or refreshed beside the old copy)
	ClipBytes     int64    `json:"clip_bytes"`     // custom regions to clip (parent × area share)
	InputBytes    int64    `json:"input_bytes"`    // OSM input of the build (extracts + clips)
	MergeBytes    int64    `json:"merge_bytes"`    // merged copy (several regions only)
	WorkBytes     int64    `json:"work_bytes"`     // input × workFactor
	TerrainBytes  int64    `json:"terrain_bytes"`
	SourcesBytes  int64    `json:"sources_bytes"` // Planetiler sources not cached yet
	Factor        float64  `json:"factor"`        // MAPTHING_DISK_GUARD_FACTOR
	NeedBytes     int64    `json:"need_bytes"`
	FreeBytes     int64    `json:"free_bytes"`
	Items         []string `json:"items"` // one line per contribution, for the job log
}

// Estimate is EstimateFor without refresh.
func (b *Builder) Estimate(regions []catalog.Region) DiskEstimate {
	return b.EstimateFor(Options{Regions: regions})
}

// EstimateFor computes the disk a build needs:
//   - each source extract once (shared parents count once), only if it must be downloaded;
//   - each custom clip at parent bytes × (clip ∩ parent area / parent area);
//   - the build input (extracts + clips), a merged copy of it, and workFactor × input of scratch;
//   - the terrain estimate and Planetiler sources that are not cached (none for the ocean
//     polygons while their host is known to be down: the Natural Earth fallback is used);
//
// all times MAPTHING_DISK_GUARD_FACTOR (default 1).
func (b *Builder) EstimateFor(opt Options) DiskEstimate {
	e := DiskEstimate{Factor: guardFactor()}
	item := func(format string, args ...any) { e.Items = append(e.Items, fmt.Sprintf(format, args...)) }

	extractSize := func(r catalog.Region) (int64, string) {
		if st, err := os.Stat(b.regionFile(r.ID)); err == nil {
			return st.Size(), "on disk"
		}
		if r.SizeHint > 0 {
			return r.SizeHint, "catalog"
		}
		return unknownPBFBytes, "size unknown"
	}
	seen := map[string]bool{}
	download := func(r catalog.Region) int64 {
		size, how := extractSize(r)
		if seen[r.ID] {
			return size
		}
		seen[r.ID] = true
		_, statErr := os.Stat(b.regionFile(r.ID))
		refresh := opt.Refresh && !opt.SkipRefresh[r.ID]
		switch {
		case statErr != nil:
			e.DownloadBytes += size
			item("download %s: %s (%s)", r.ID, humanBytes(size), how)
		case refresh:
			e.DownloadBytes += size
			item("refresh %s: %s (new copy beside the old)", r.ID, humanBytes(size))
		default:
			item("reuse %s: %s on disk", r.ID, humanBytes(size))
		}
		return size
	}
	for _, r := range opt.Regions {
		if r.Custom == nil {
			e.InputBytes += download(r)
			continue
		}
		parent := r.Custom.Parent
		parentBytes := download(parent)
		if st, err := os.Stat(b.regionFile(r.ID)); err == nil {
			e.InputBytes += st.Size()
			item("clip %s: %s on disk", r.ID, humanBytes(st.Size()))
			continue
		}
		frac := clipFraction(r, parent)
		clip := max(int64(float64(parentBytes)*frac), 1<<20)
		e.ClipBytes += clip
		e.InputBytes += clip
		item("clip %s: %s (%.1f%% of %s)", r.ID, humanBytes(clip), frac*100, parent.ID)
	}
	if len(opt.Regions) > 1 {
		e.MergeBytes = e.InputBytes
		item("merged input: %s", humanBytes(e.MergeBytes))
	}
	e.WorkBytes = e.InputBytes * workFactor
	item("build scratch and outputs: %s (input %s × %d)", humanBytes(e.WorkBytes), humanBytes(e.InputBytes), workFactor)
	if f := hooks.terrainEstimate(); f != nil {
		e.TerrainBytes = f(opt.Regions)
		item("terrain: %s", humanBytes(e.TerrainBytes))
	}
	for _, src := range planetilerSources {
		if _, err := os.Stat(filepath.Join(b.sourcesDir(), src.file)); err == nil {
			continue
		}
		if src.ocean {
			if _, down := b.hostFailedAt(hostOf(src.url)); down && os.Getenv(src.env) == "" {
				item("planetiler %s: 0 (host down; Natural Earth fallback)", src.name)
				continue
			}
		}
		e.SourcesBytes += src.approxBytes
		item("planetiler %s: %s (first download)", src.name, humanBytes(src.approxBytes))
	}
	raw := e.DownloadBytes + e.ClipBytes + e.MergeBytes + e.WorkBytes + e.TerrainBytes + e.SourcesBytes
	e.NeedBytes = int64(float64(raw) * e.Factor)
	if e.Factor != 1 {
		item("× MAPTHING_DISK_GUARD_FACTOR %.2f", e.Factor)
	}
	e.FreeBytes = freeBytes(b.DataDir)
	return e
}

// guardFactor reads MAPTHING_DISK_GUARD_FACTOR (default 1; invalid or ≤ 0 → 1).
func guardFactor() float64 {
	if v := os.Getenv("MAPTHING_DISK_GUARD_FACTOR"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 1
}

// clipFraction is the share of the parent extract's area that the clip covers, estimated on a
// grid over the clip's bbox (cells weighted by cos(latitude), so it is area, not degrees²).
func clipFraction(clip, parent catalog.Region) float64 {
	clipPoly := clip.Polygon
	if len(clipPoly) == 0 {
		b := clip.BBox
		clipPoly = [][][2]float64{{{b[0], b[1]}, {b[2], b[1]}, {b[2], b[3]}, {b[0], b[3]}, {b[0], b[1]}}}
	}
	parentPoly := parent.Polygon
	if len(parentPoly) == 0 {
		b := parent.BBox
		parentPoly = [][][2]float64{{{b[0], b[1]}, {b[2], b[1]}, {b[2], b[3]}, {b[0], b[3]}, {b[0], b[1]}}}
	}
	pArea := gridArea(polyBBox(parentPoly), func(p [2]float64) bool { return evenOdd(parentPoly, p) })
	if pArea <= 0 {
		return 1
	}
	iArea := gridArea(polyBBox(clipPoly), func(p [2]float64) bool { return evenOdd(clipPoly, p) && evenOdd(parentPoly, p) })
	return min(max(iArea/pArea, 0), 1)
}

func gridArea(b [4]float64, inside func([2]float64) bool) float64 {
	const n = 60
	w, h := (b[2]-b[0])/n, (b[3]-b[1])/n
	var a float64
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			p := [2]float64{b[0] + w*(float64(i)+0.5), b[1] + h*(float64(j)+0.5)}
			if inside(p) {
				a += w * h * math.Cos(p[1]*math.Pi/180)
			}
		}
	}
	return a
}

func polyBBox(poly [][][2]float64) [4]float64 {
	b := [4]float64{180, 90, -180, -90}
	for _, r := range poly {
		for _, p := range r {
			b[0], b[1] = math.Min(b[0], p[0]), math.Min(b[1], p[1])
			b[2], b[3] = math.Max(b[2], p[0]), math.Max(b[3], p[1])
		}
	}
	return b
}

// evenOdd is the even-odd point-in-polygon test over every ring.
func evenOdd(poly [][][2]float64, pt [2]float64) bool {
	in := false
	for _, ring := range poly {
		for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
			a, b := ring[i], ring[j]
			if (a[1] > pt[1]) != (b[1] > pt[1]) && pt[0] < (b[0]-a[0])*(pt[1]-a[1])/(b[1]-a[1])+a[0] {
				in = !in
			}
		}
	}
	return in
}

func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * st.Bsize
}

func (s *state) guard(ctx context.Context) error {
	if err := os.MkdirAll(s.b.DataDir, 0o755); err != nil {
		return err
	}
	e := s.b.EstimateFor(s.opt)
	s.m.Disk = &e
	for _, it := range e.Items {
		s.logf("disk: %s", it)
	}
	s.logf("disk: need ~%s, free %s", humanBytes(e.NeedBytes), humanBytes(e.FreeBytes))
	if e.FreeBytes >= 0 && e.FreeBytes < e.NeedBytes {
		return fmt.Errorf("not enough disk on %s: this build needs about %s but only %s is free; "+
			"remove regions, grow the volume, or set MAPTHING_DISK_GUARD_FACTOR below 1 to accept the risk",
			s.b.DataDir, humanBytes(e.NeedBytes), humanBytes(e.FreeBytes))
	}
	mb := memoryBudget()
	s.m.Memory = &mb
	if mem := mb.Effective; mem > 0 {
		s.logf("memory available: %s (host %s, container limit %s, %s)", humanBytes(mem),
			humanBytes(mb.HostAvailable), humanBytes(mb.CgroupLimit), mb.Source)
		if mem < 2<<30 {
			return fmt.Errorf("only %s of memory available; a build needs at least 2 GiB", humanBytes(mem))
		}
	}
	return nil
}
