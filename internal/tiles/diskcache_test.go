package tiles

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDiskCacheLRU(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"g1/a", "g1/b"} {
		if _, err := c.PutBytes(k, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
	}
	c.Get("g1/a") // a is now most recent
	if _, err := c.PutBytes("g2/c", make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("g1/b"); ok {
		t.Fatal("least recently used entry not evicted")
	}
	if _, ok := c.Get("g1/a"); !ok {
		t.Fatal("recent entry evicted")
	}
	if c.Size() != 20 {
		t.Fatalf("size %d", c.Size())
	}
	// reload from disk
	c2, err := NewDiskCache(dir, 25)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Size() != 20 {
		t.Fatalf("reloaded size %d", c2.Size())
	}
	c2.RemoveIf(func(k string) bool { return k[:3] == "g1/" })
	if _, ok := c2.Get("g1/a"); ok {
		t.Fatal("RemoveIf kept entry")
	}
	if _, err := os.Stat(dir + "/g1"); !os.IsNotExist(err) {
		t.Fatal("empty dir not removed")
	}
	for _, bad := range []string{"../x", "/abs", "a//b", ""} {
		if _, err := c2.PutBytes(bad, nil); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}

func TestDiskCacheDoSingleFlight(t *testing.T) {
	c, err := NewDiskCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var fills atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := c.Do("k", func() error {
				fills.Add(1)
				_, err := c.PutBytes("k", []byte("v"))
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fills.Load(); n != 1 {
		t.Fatalf("filled %d times", n)
	}
	if _, _, err := c.Do("bad", func() error { return errors.New("upstream down") }); err == nil {
		t.Fatal("expected error")
	}
}
