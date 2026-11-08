package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// planetilerMemory sizes the JVM heap and Planetiler storage from available memory and the
// extract size: 2 GiB plus 6x the extract, capped at 60% of available memory (1–32 GiB); node
// locations and features stay in RAM while they comfortably fit, otherwise they go to
// memory-mapped files on disk.
func planetilerMemory(avail, pbf int64) (heapMB int64, storage string) {
	heap := min(avail*6/10, 2<<30+pbf*6)
	if heap < 1<<30 {
		heap = 1 << 30
	}
	if heap > 32<<30 {
		heap = 32 << 30
	}
	storage = "ram"
	// Planetiler needs roughly 1.5x the PBF size of heap to keep everything in RAM.
	if pbf*3/2 > heap*7/10 {
		storage = "mmap"
	}
	return heap >> 20, storage
}

func (s *state) basemap(ctx context.Context) error {
	if err := os.MkdirAll(s.genDir, 0o755); err != nil {
		return err
	}
	// The Protomaps profile reads its auxiliary sources from ./data/sources relative to the
	// working directory; point that at the shared cache so they download once.
	wd := filepath.Join(s.workDir, "planetiler")
	if err := os.MkdirAll(filepath.Join(wd, "data"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(s.b.sourcesDir(), 0o755); err != nil {
		return err
	}
	link := filepath.Join(wd, "data", "sources")
	_ = os.Remove(link)
	if err := os.Symlink(s.b.sourcesDir(), link); err != nil {
		return err
	}
	var pbf int64
	if st, err := os.Stat(s.merged); err == nil {
		pbf = st.Size()
	}
	avail := memAvailable()
	if avail == 0 {
		avail = 4 << 30
	}
	heapMB, storage := planetilerMemory(avail, pbf)
	out := filepath.Join(s.genDir, "basemap.pmtiles")
	threads := runtime.NumCPU()
	s.logf("planetiler: heap %d MiB, storage %s, %d threads", heapMB, storage, threads)
	args := []string{
		fmt.Sprintf("-Xmx%dm", heapMB),
		"-XX:+UseParallelGC",
		"-jar", s.b.Tools.PlanetilerJar,
		"--osm_path=" + s.merged,
		"--output=" + out,
		"--force",
		"--download",
		"--maxzoom=15",
		"--tmpdir=" + filepath.Join(wd, "tmp"),
		"--storage=" + storage,
		"--nodemap-type=sparsearray",
		"--nodemap-storage=" + storage,
		fmt.Sprintf("--threads=%d", threads),
	}
	ocean, waterZip, landZip, err := s.ensureSources(ctx, wd)
	if err != nil {
		return err
	}
	s.m.Ocean = ocean
	if waterZip != "" {
		args = append(args, "--osm_water_path="+waterZip, "--osm_land_path="+landZip)
	}
	if err := s.exec(ctx, wd, s.b.Tools.Java, args...); err != nil {
		return err
	}
	st, err := os.Stat(out)
	if err != nil {
		return fmt.Errorf("planetiler produced no output: %w", err)
	}
	s.m.Artifacts["basemap.pmtiles"] = st.Size()
	return nil
}
