package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ValhallaConfig returns the generation's valhalla.json path.
func ValhallaConfig(genDir string) string { return filepath.Join(genDir, "valhalla", "valhalla.json") }

func (s *state) valhalla(ctx context.Context) error {
	vdir := filepath.Join(s.genDir, "valhalla")
	tileDir := filepath.Join(s.workDir, "valhalla-tiles")
	for _, d := range []string{vdir, tileDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	tz := s.timezones(ctx)
	extract := filepath.Join(vdir, "tiles.tar")
	admin := filepath.Join(s.workDir, "valhalla-admins.sqlite")
	cfgArgs := []string{
		"--mjolnir-tile-dir", tileDir,
		"--mjolnir-tile-extract", extract,
		"--mjolnir-admin", admin,
		"--mjolnir-timezone", tz,
		"--mjolnir-traffic-extract", "",
		"--httpd-service-listen", "tcp://127.0.0.1:8002",
	}
	var out bytes.Buffer
	if _, err := s.execOut(ctx, vdir, &out, s.b.Tools.ValhallaBuildConfig, cfgArgs...); err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		return fmt.Errorf("valhalla_build_config output: %w", err)
	}
	if p := s.b.Tools.ValhallaOverrides; p != "" {
		if data, err := os.ReadFile(p); err == nil {
			var ov map[string]any
			if err := json.Unmarshal(data, &ov); err != nil {
				return fmt.Errorf("valhalla overrides %s: %w", p, err)
			}
			deepMerge(cfg, ov)
			s.logf("valhalla: merged overrides from %s", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	// Our paths win over anything an override says.
	setPath(cfg, []string{"mjolnir", "tile_dir"}, tileDir)
	setPath(cfg, []string{"mjolnir", "tile_extract"}, extract)
	setPath(cfg, []string{"mjolnir", "admin"}, admin)
	setPath(cfg, []string{"mjolnir", "timezone"}, tz)
	cfgPath := ValhallaConfig(s.genDir)
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return err
	}

	s.rep.Progress(0.05, "admin boundaries")
	if err := s.exec(ctx, s.workDir, s.b.Tools.ValhallaBuildAdmins, "-c", cfgPath, s.merged); err != nil {
		return err
	}
	s.rep.Progress(0.15, "graph tiles")
	if err := s.exec(ctx, s.workDir, s.b.Tools.ValhallaBuildTiles, "-c", cfgPath, s.merged); err != nil {
		return err
	}
	s.rep.Progress(0.9, "tile extract")
	if err := s.exec(ctx, s.workDir, s.b.Tools.ValhallaBuildExtract, "-c", cfgPath, "-O", "-v"); err != nil {
		return err
	}
	st, err := os.Stat(extract)
	if err != nil {
		return fmt.Errorf("valhalla_build_extract produced no tar: %w", err)
	}
	s.m.Artifacts["valhalla/tiles.tar"] = st.Size()
	// The loose tiles are in the work dir and go away with it; the tar is what valhalla_service
	// reads. Admin and timezone DBs are only needed while building tiles.
	return nil
}

// timezones returns a cached timezone database path, building it once. Failure is not fatal:
// Valhalla then builds tiles without time zones (time-dependent routing is less accurate).
func (s *state) timezones(ctx context.Context) string {
	dir := filepath.Join(s.b.DataDir, "sources", "valhalla")
	p := filepath.Join(dir, "timezones.sqlite")
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		return p
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return p
	}
	f, err := os.Create(p + ".part")
	if err != nil {
		return p
	}
	_, err = s.execOut(ctx, dir, f, "valhalla_build_timezones")
	f.Close()
	if err != nil {
		s.logf("valhalla: timezone db unavailable (%v); continuing without", err)
		os.Remove(p + ".part")
		return p
	}
	os.Rename(p+".part", p)
	return p
}

// deepMerge copies src into dst, recursing into objects; non-object values replace.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func setPath(m map[string]any, path []string, v any) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}
