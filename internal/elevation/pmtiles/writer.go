package pmtiles

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// Writer builds an archive. Tiles may be added in any order, from several
// goroutines; tile data is streamed to disk as it arrives (the archive is
// written unclustered), and identical tiles are stored once.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	pos      uint64 // next tile-data offset, relative to the data section
	entries  []Entry
	seen     map[[32]byte]Entry
	contents uint64
	closed   bool
}

// dataStart leaves room for the header and a root directory of up to
// rootMaxSize bytes, so tile data can be streamed before directories exist.
const dataStart = 16384

// Create starts a new archive at path (written to path+".part" until Finish).
func Create(path string) (*Writer, error) {
	f, err := os.Create(path + ".part")
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(dataStart, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{f: f, path: path, seen: map[[32]byte]Entry{}}, nil
}

// Add stores one tile. Adding the same z/x/y twice is an error at Finish.
func (w *Writer) Add(z uint8, x, y uint32, data []byte) error {
	id := ZxyToID(z, x, y)
	sum := sha256.Sum256(data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("pmtiles: writer closed")
	}
	if e, ok := w.seen[sum]; ok {
		w.entries = append(w.entries, Entry{TileID: id, Offset: e.Offset, Length: e.Length, RunLength: 1})
		return nil
	}
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	e := Entry{TileID: id, Offset: w.pos, Length: uint32(len(data)), RunLength: 1}
	w.pos += uint64(len(data))
	w.seen[sum] = e
	w.entries = append(w.entries, e)
	w.contents++
	return nil
}

// Count returns the number of tiles added so far.
func (w *Writer) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entries)
}

// Abort discards the partial archive.
func (w *Writer) Abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		w.f.Close()
		os.Remove(w.path + ".part")
	}
}

// Finish writes directories, metadata and header, and renames the archive into
// place. h supplies tile type, compression, zooms, bounds and center; offsets
// and counts are filled in here. metadata is marshalled to JSON.
func (w *Writer) Finish(h Header, metadata any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("pmtiles: writer closed")
	}
	w.closed = true
	fail := func(err error) error {
		w.f.Close()
		os.Remove(w.path + ".part")
		return err
	}

	sort.Slice(w.entries, func(i, j int) bool { return w.entries[i].TileID < w.entries[j].TileID })
	// Merge runs: consecutive tile IDs that point at the same bytes.
	var merged []Entry
	for i, e := range w.entries {
		if i > 0 && e.TileID == w.entries[i-1].TileID {
			return fail(fmt.Errorf("pmtiles: tile id %d added twice", e.TileID))
		}
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if last.Offset == e.Offset && last.Length == e.Length && last.TileID+uint64(last.RunLength) == e.TileID {
				last.RunLength++
				continue
			}
		}
		merged = append(merged, e)
	}

	meta, err := json.Marshal(metadata)
	if err != nil {
		return fail(err)
	}
	metaGz, err := gzipBytes(meta)
	if err != nil {
		return fail(err)
	}

	root, leaves, err := buildDirectories(merged)
	if err != nil {
		return fail(err)
	}

	h.RootOffset = headerLen
	h.RootLength = uint64(len(root))
	h.DataOffset = dataStart
	h.DataLength = w.pos
	h.MetadataOffset = dataStart + w.pos
	h.MetadataLength = uint64(len(metaGz))
	h.LeafOffset = h.MetadataOffset + h.MetadataLength
	h.LeafLength = uint64(len(leaves))
	h.AddressedTiles = uint64(len(w.entries))
	h.TileEntries = uint64(len(merged))
	h.TileContents = w.contents
	h.Clustered = false
	h.InternalCompression = CompressionGzip

	if _, err := w.f.Write(metaGz); err != nil {
		return fail(err)
	}
	if _, err := w.f.Write(leaves); err != nil {
		return fail(err)
	}
	if _, err := w.f.WriteAt(h.marshal(), 0); err != nil {
		return fail(err)
	}
	if _, err := w.f.WriteAt(root, headerLen); err != nil {
		return fail(err)
	}
	if err := w.f.Sync(); err != nil {
		return fail(err)
	}
	if err := w.f.Close(); err != nil {
		os.Remove(w.path + ".part")
		return err
	}
	return os.Rename(w.path+".part", w.path)
}

// buildDirectories returns a root directory that fits in the reserved space
// and, if needed, one level of leaf directories.
func buildDirectories(entries []Entry) (root, leaves []byte, err error) {
	root, err = serializeEntries(entries)
	if err != nil {
		return nil, nil, err
	}
	if len(root) <= rootMaxSize {
		return root, nil, nil
	}
	for leafSize := 4096; ; leafSize *= 2 {
		var rootEntries []Entry
		var buf []byte
		for i := 0; i < len(entries); i += leafSize {
			end := min(i+leafSize, len(entries))
			b, err := serializeEntries(entries[i:end])
			if err != nil {
				return nil, nil, err
			}
			rootEntries = append(rootEntries, Entry{TileID: entries[i].TileID, Offset: uint64(len(buf)), Length: uint32(len(b))})
			buf = append(buf, b...)
		}
		root, err = serializeEntries(rootEntries)
		if err != nil {
			return nil, nil, err
		}
		if len(root) <= rootMaxSize {
			return root, buf, nil
		}
	}
}
