package tiles

import "github.com/jclement/mapthing/internal/diskcache"

// DiskCache is the bounded on-disk cache (internal/diskcache) the tile server keeps rendered
// tiles, imagery and packs in.
type DiskCache = diskcache.Cache

// NewDiskCache opens (creating) dir with a byte budget (<= 0 disables storing).
func NewDiskCache(dir string, maxBytes int64) (*DiskCache, error) {
	return diskcache.New(dir, maxBytes)
}
