package build

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jclement/mapthing/internal/catalog"
)

// planetilerSourcesBytes approximates the one-off Planetiler auxiliary downloads (Natural Earth,
// OSM water and land polygons, Daylight landcover, QRank), cached under /data/sources.
const planetilerSourcesBytes = 4 << 30

// unknownPBFBytes is assumed for an extract whose size we do not know yet.
const unknownPBFBytes = 512 << 20

// DiskEstimate is what a build is expected to need on the data volume.
type DiskEstimate struct {
	PBFBytes     int64 `json:"pbf_bytes"`
	TerrainBytes int64 `json:"terrain_bytes"`
	SourcesBytes int64 `json:"sources_bytes"`
	NeedBytes    int64 `json:"need_bytes"`
	FreeBytes    int64 `json:"free_bytes"`
}

// Estimate computes the disk a build of regions needs: about six times the extract size
// (download, merge, planetiler scratch and output, valhalla tiles and tar) plus the terrain
// estimate and any missing Planetiler auxiliary sources.
func (b *Builder) Estimate(regions []catalog.Region) DiskEstimate {
	var e DiskEstimate
	for _, r := range regions {
		size := r.SizeHint
		if st, err := os.Stat(b.regionFile(r.ID)); err == nil {
			size = st.Size()
		}
		if size <= 0 {
			size = unknownPBFBytes
		}
		e.PBFBytes += size
	}
	if f := hooks.terrainEstimate(); f != nil {
		e.TerrainBytes = f(regions)
	}
	if !sourcesPresent(b.sourcesDir()) {
		e.SourcesBytes = planetilerSourcesBytes
	}
	e.NeedBytes = e.PBFBytes*6 + e.TerrainBytes + e.SourcesBytes
	e.FreeBytes = freeBytes(b.DataDir)
	return e
}

func sourcesPresent(dir string) bool {
	for _, f := range []string{"water-polygons-split-3857.zip", "land-polygons-split-3857.zip", "natural_earth_vector.gpkg.zip"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// memAvailable returns MemAvailable from /proc/meminfo, or 0.
func memAvailable() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb * 1024
		}
	}
	return 0
}

func (s *state) guard(ctx context.Context) error {
	if err := os.MkdirAll(s.b.DataDir, 0o755); err != nil {
		return err
	}
	e := s.b.Estimate(s.opt.Regions)
	s.m.Disk = &e
	s.logf("disk: need ~%s (extracts %s ×6, terrain %s, planetiler sources %s), free %s",
		humanBytes(e.NeedBytes), humanBytes(e.PBFBytes), humanBytes(e.TerrainBytes),
		humanBytes(e.SourcesBytes), humanBytes(e.FreeBytes))
	if e.FreeBytes >= 0 && e.FreeBytes < e.NeedBytes {
		return fmt.Errorf("not enough disk on %s: this build needs about %s but only %s is free; "+
			"remove regions or grow the volume", s.b.DataDir, humanBytes(e.NeedBytes), humanBytes(e.FreeBytes))
	}
	if mem := memAvailable(); mem > 0 {
		s.logf("memory available: %s", humanBytes(mem))
		if mem < 2<<30 {
			return fmt.Errorf("only %s of memory available; a build needs at least 2 GiB", humanBytes(mem))
		}
	}
	return nil
}
