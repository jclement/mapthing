package build

// Reusing a previous generation's terrain (Options.ReuseTerrain).
//
// Terrain is the slow stage and its input (public DEMs) changes rarely, unlike OSM. A terrain
// build records a fingerprint of everything that decides its content next to the archive
// (terrain.pmtiles.json). A later build for the same areas with the same settings links or
// copies that archive instead of rebuilding it; contours follow when their setting matches,
// and are otherwise rebuilt from the reused terrain (a local computation, no downloads).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jclement/mapthing/internal/catalog"
	"github.com/jclement/mapthing/internal/elevation"
	"github.com/jclement/mapthing/internal/pmtiles"
)

// terrainFormat changes when the terrain build's output changes for the same inputs (encoding,
// resampling, source handling), so archives from an older build are not reused.
const terrainFormat = 1

// TerrainFingerprint identifies the terrain archive a build of regions would produce with the
// current settings: the areas (ids, boxes, outlines), zoom range, rounding step and the DEM
// sources in priority order. It does not cover the DEM data itself, which the build caches.
func TerrainFingerprint(regions []catalog.Region) string {
	return terrainFingerprint(areasFromRegions(regions), terrainOptsFromEnv())
}

func terrainFingerprint(areas []area, o terrainOpts) string {
	type fpArea struct {
		ID      string         `json:"id"`
		BBox    [4]float64     `json:"bbox"`
		Polygon [][][2]float64 `json:"polygon,omitempty"`
	}
	z, _ := chooseMaxZoom(areas, o)
	v := struct {
		Format   int      `json:"format"`
		TileSize int      `json:"tile_size"`
		MinZoom  int      `json:"minzoom"`
		MaxZoom  int      `json:"maxzoom"`
		Quantum  float64  `json:"quantum_m"`
		Sources  []string `json:"sources"`
		Areas    []fpArea `json:"areas"`
	}{Format: terrainFormat, TileSize: elevation.TileSize, MinZoom: o.minZoom, MaxZoom: z, Quantum: o.quantum}
	for _, s := range demSources() {
		v.Sources = append(v.Sources, s.def.ID)
	}
	for _, a := range areas {
		fa := fpArea{ID: a.id, BBox: [4]float64{round6(a.bbox[0]), round6(a.bbox[1]), round6(a.bbox[2]), round6(a.bbox[3])}}
		for _, ring := range a.polygon {
			rr := make([][2]float64, len(ring))
			for i, p := range ring {
				rr[i] = [2]float64{round6(p[0]), round6(p[1])}
			}
			fa.Polygon = append(fa.Polygon, rr)
		}
		v.Areas = append(v.Areas, fa)
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readTerrainFingerprint returns the fingerprint recorded beside a terrain archive, or "".
func readTerrainFingerprint(terrainPath string) string {
	var r struct {
		Fingerprint string `json:"fingerprint"`
	}
	if b, err := os.ReadFile(terrainPath + ".json"); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r.Fingerprint
}

// contoursInfo is contours.pmtiles.json: what the contour archive was built from.
type contoursInfo struct {
	TerrainFingerprint string `json:"terrain_fingerprint"`
	Setting            string `json:"setting"` // MAPTHING_CONTOURS_MAXZOOM, or "default"
}

func contoursSetting() string {
	if v := os.Getenv(envContoursMaxZoom); v != "" {
		return v
	}
	return "default"
}

// writeContoursInfo records which terrain and setting a contour build used, so a later build
// can tell whether the contours can be reused along with the terrain. Best effort.
func writeContoursInfo(terrainPath, contoursPath string) {
	fp := readTerrainFingerprint(terrainPath)
	if fp == "" {
		return
	}
	b, _ := json.MarshalIndent(contoursInfo{TerrainFingerprint: fp, Setting: contoursSetting()}, "", "  ")
	_ = os.WriteFile(contoursPath+".json", b, 0o644)
}

// reuseTerrain links or copies terrain.pmtiles from a previous generation directory to out
// when it was built for the same areas and settings. It returns "" when the terrain was
// reused, otherwise why not (the caller then builds it). An error means the reuse started and
// failed half way.
func (s *state) reuseTerrain(dir, out string) (string, error) {
	fpf := hooks.fingerprintFunc()
	if fpf == nil {
		return "this build cannot fingerprint terrain", nil
	}
	src := filepath.Join(dir, "terrain.pmtiles")
	if _, err := os.Stat(src); err != nil {
		return "no terrain.pmtiles there", nil
	}
	have := readTerrainFingerprint(src)
	if have == "" {
		return "its terrain has no recorded fingerprint (built by an older version)", nil
	}
	if want := fpf(s.opt.Regions); have != want {
		return "its terrain was built for other areas or settings", nil
	}
	r, err := pmtiles.Open(src)
	if err != nil {
		return "its terrain archive does not open: " + err.Error(), nil
	}
	_ = r.Close()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	how, err := linkOrCopy(src, out)
	if err != nil {
		return "", fmt.Errorf("reuse terrain: %w", err)
	}
	if _, err := linkOrCopy(src+".json", out+".json"); err != nil {
		return "", fmt.Errorf("reuse terrain: %w", err)
	}
	s.logf("terrain: reused from %s (%s, fingerprint %s)", dir, how, have[:12])
	return "", nil
}

// reuseContours links or copies contours.pmtiles built from the reused terrain with the same
// setting. false means the caller must build contours.
func (s *state) reuseContours(dir, terrainOut, out string) bool {
	src := filepath.Join(dir, "contours.pmtiles")
	var info contoursInfo
	b, err := os.ReadFile(src + ".json")
	if err != nil || json.Unmarshal(b, &info) != nil {
		return false
	}
	if info.Setting != contoursSetting() || info.TerrainFingerprint != readTerrainFingerprint(terrainOut) {
		return false
	}
	if _, err := os.Stat(src); err != nil {
		// Contours were disabled then and are disabled now: nothing to build either.
		if info.Setting == "0" {
			_, _ = linkOrCopy(src+".json", out+".json")
			return true
		}
		return false
	}
	how, err := linkOrCopy(src, out)
	if err != nil {
		_ = os.Remove(out)
		return false
	}
	_, _ = linkOrCopy(src+".json", out+".json")
	s.logf("contours: reused from %s (%s)", dir, how)
	return true
}

// linkOrCopy hard-links src to dst, or copies it when they are on different filesystems.
func linkOrCopy(src, dst string) (string, error) {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return "hard link", nil
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, in); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return "copy", os.Rename(tmp, dst)
}
