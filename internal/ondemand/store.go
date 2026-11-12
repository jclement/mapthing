package ondemand

import (
	"os"
	"strings"

	"github.com/jclement/mapthing/internal/diskcache"
)

// diskStore keeps the service's bytes in a bounded disk cache: finished
// tiles, the source blocks they were made from and file headers share one
// budget, least recently used first out.
type diskStore struct{ c *diskcache.Cache }

// NewDiskStore adapts a disk cache to the Store the service needs.
func NewDiskStore(c *diskcache.Cache) Store { return diskStore{c} }

func (d diskStore) Get(key string) ([]byte, bool) {
	p, ok := d.c.Get(key)
	if !ok {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (d diskStore) Put(key string, b []byte) error {
	_, err := d.c.PutBytes(key, b)
	return err
}

func (d diskStore) RemovePrefix(prefix string) {
	d.c.RemoveIf(func(k string) bool { return strings.HasPrefix(k, prefix) })
}
