package ondemand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/jclement/mapthing/internal/diskcache"
	"github.com/jclement/mapthing/internal/elevation"
)

// Outside settings.
const (
	OutsideOff      = "off"
	OutsideOnDemand = "on_demand"
)

// Config is what the operator sets (admin Settings page). The per-region
// choice lives with each region.
type Config struct {
	// Outside is OutsideOff (the default: places outside every built region
	// have no terrain) or OutsideOnDemand (they are read on demand).
	Outside string `json:"outside"`
	// CacheMB is the disk budget of the on-demand cache, in megabytes
	// (decimal, as the admin pages show sizes: 5000 is 5 GB).
	CacheMB int64 `json:"cache_mb"`
}

// DefaultCacheMB is the default budget of the on-demand cache.
const DefaultCacheMB = 5000

// Bytes is the budget in bytes.
func (c Config) Bytes() int64 { return c.CacheMB * 1_000_000 }

// DefaultConfig is on demand off outside regions, with a 5 GB cache.
func DefaultConfig() Config { return Config{Outside: OutsideOff, CacheMB: DefaultCacheMB} }

// Validate checks a configuration from the admin API.
func (c Config) Validate() error {
	if c.Outside != OutsideOff && c.Outside != OutsideOnDemand {
		return errors.New(`outside must be "off" or "on_demand"`)
	}
	if c.CacheMB < 64 || c.CacheMB > 4_000_000 {
		return errors.New("cache_mb must be between 64 and 4000000")
	}
	return nil
}

// Settings is where the configuration is kept (the store's settings table).
type Settings interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

const configSetting = "ondemand.config"

// ManagerOptions configure a Manager.
type ManagerOptions struct {
	DataDir  string
	Settings Settings
	// Areas returns the regions whose terrain is set to on demand.
	Areas func(ctx context.Context) ([]Area, error)
	// Service overrides the service options (tests); Store and StatePath are filled in.
	Service Options
	// Logf, if set, receives a line when a warm job ends.
	Logf func(format string, args ...any)
}

// Manager owns on-demand terrain for a server: the service, its disk cache,
// the operator's configuration and the policy that follows from it and from
// the regions.
type Manager struct {
	Svc   *Service
	cache *diskcache.Cache
	opt   ManagerOptions

	mu  sync.Mutex
	cfg Config

	// built reports whether the live terrain archive has a tile (built terrain
	// wins, so a warm job leaves those alone). nil = nothing is built.
	built atomic.Pointer[func(z, x, y int) bool]

	warmMu     sync.Mutex
	warm       WarmStatus
	warmCancel context.CancelFunc
}

// SetBuilt tells the manager how to ask whether a terrain tile is built.
func (m *Manager) SetBuilt(f func(z, x, y int) bool) { m.built.Store(&f) }

// NewManager opens the cache under <data>/ondemand-cache and applies the
// stored configuration.
func NewManager(ctx context.Context, opt ManagerOptions) (*Manager, error) {
	m := &Manager{opt: opt, cfg: DefaultConfig()}
	if opt.Settings != nil {
		if v, err := opt.Settings.Setting(ctx, configSetting); err == nil {
			var c Config
			if json.Unmarshal([]byte(v), &c) == nil && c.Validate() == nil {
				m.cfg = c
			}
		}
	}
	var err error
	if m.cache, err = diskcache.New(filepath.Join(opt.DataDir, "ondemand-cache"), m.cfg.Bytes()); err != nil {
		return nil, fmt.Errorf("on-demand cache: %w", err)
	}
	so := opt.Service
	so.Store = NewDiskStore(m.cache)
	if so.StatePath == "" {
		so.StatePath = filepath.Join(opt.DataDir, "ondemand", "versions.json")
	}
	m.Svc = New(so)
	m.Refresh(ctx)
	return m, nil
}

// Config returns the current configuration.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// SetConfig validates, stores and applies a configuration.
func (m *Manager) SetConfig(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if m.opt.Settings != nil {
		b, _ := json.Marshal(c)
		if err := m.opt.Settings.SetSetting(ctx, configSetting, string(b)); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
	m.cache.SetMax(c.Bytes())
	m.Refresh(ctx)
	return nil
}

// Refresh recomputes where on demand applies. Call it when the regions change.
func (m *Manager) Refresh(ctx context.Context) {
	p := Policy{Outside: m.Config().Outside == OutsideOnDemand}
	if m.opt.Areas != nil {
		if areas, err := m.opt.Areas(ctx); err == nil {
			p.Areas = areas
		}
	}
	m.Svc.SetPolicy(p)
}

// Enabled implements the tile server's on-demand hook.
func (m *Manager) Enabled() bool { return m.Svc.Policy().Enabled() }

// Allows implements the tile server's on-demand hook.
func (m *Manager) Allows(z, x, y int) bool { return m.Svc.Policy().AllowsTile(z, x, y) }

// Tile implements the tile server's on-demand hook.
func (m *Manager) Tile(ctx context.Context, z, x, y int) ([]byte, bool, error) {
	return m.Svc.Tile(ctx, z, x, y)
}

// Bounds returns the box on demand applies to (the world, or the union of
// the on-demand regions), and false when it applies nowhere.
func (m *Manager) Bounds() (elevation.BBox, bool) {
	p := m.Svc.Policy()
	if p.Outside {
		return elevation.BBox{-180, -85.0511, 180, 85.0511}, true
	}
	if len(p.Areas) == 0 {
		return elevation.BBox{}, false
	}
	b := p.Areas[0].BBox
	for _, a := range p.Areas[1:] {
		b = elevation.BBox{min(b[0], a.BBox[0]), min(b[1], a.BBox[1]), max(b[2], a.BBox[2]), max(b[3], a.BBox[3])}
	}
	return b, true
}

// CacheBytes is the disk the cache uses now.
func (m *Manager) CacheBytes() int64 { return m.cache.Size() }

// Clear empties the cache. Tiles are fetched again when next asked for.
func (m *Manager) Clear() {
	m.cache.RemoveIf(func(string) bool { return true })
	m.Svc.decoded.clear()
}

// SourceStatus describes one DEM source for the admin pages.
type SourceStatus struct {
	elevation.SourceInfo
	Host string `json:"host"`
}

// Status is what the admin API reports.
type Status struct {
	Config  Config `json:"config"`
	Enabled bool   `json:"enabled"`
	// Regions lists the regions set to on demand.
	Regions []string `json:"regions"`
	MinZoom int      `json:"minzoom"`
	MaxZoom int      `json:"maxzoom"`
	Cache   struct {
		Bytes    int64 `json:"bytes"`
		MaxBytes int64 `json:"max_bytes"`
		Entries  int   `json:"entries"`
	} `json:"cache"`
	Counters Counters       `json:"counters"`
	Upstream []HostHealth   `json:"upstream"`
	Sources  []SourceStatus `json:"sources"`
	// Healthy is false while the most recent request to any DEM host failed.
	Healthy bool `json:"healthy"`
	// Warm is the latest "warm this area" job.
	Warm WarmStatus `json:"warm"`
}

// Status reports the configuration, cache use, counters and host health.
func (m *Manager) Status() Status {
	p := m.Svc.Policy()
	st := Status{Config: m.Config(), Enabled: p.Enabled(), Regions: []string{}, MinZoom: MinZoom, MaxZoom: MaxZoom,
		Counters: m.Svc.Counters(), Upstream: m.Svc.up.Health(), Healthy: true, Warm: m.WarmStatus()}
	for _, a := range p.Areas {
		st.Regions = append(st.Regions, a.ID)
	}
	sort.Strings(st.Regions)
	st.Cache.Bytes, st.Cache.MaxBytes, st.Cache.Entries = m.cache.Size(), m.cache.Max(), m.cache.Len()
	sort.Slice(st.Upstream, func(i, j int) bool { return st.Upstream[i].Host < st.Upstream[j].Host })
	for _, h := range st.Upstream {
		st.Healthy = st.Healthy && h.Healthy
	}
	for _, s := range m.Svc.sources {
		st.Sources = append(st.Sources, SourceStatus{SourceInfo: s.def.SourceInfo, Host: s.host})
	}
	return st
}
