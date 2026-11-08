package pmtiles

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
)

// Writer builds an archive. Tiles may be added in any order and from several goroutines;
// identical tile contents are stored once and consecutive identical tiles become runs. Tile
// bytes go to disk as they arrive, so memory is bounded by the directory (~32 bytes per tile).
//
// Two modes:
//   - Create(path): tile data is written in place in the final file, after a reserved
//     16 KiB header+root area; Commit writes metadata and leaf directories after the data,
//     then the header and root at the front, and renames path.part to path.
//   - NewWriter(dir): tile data is spooled to a temporary file in dir; Finish streams
//     header, root, metadata, leaves and data, in that order, to any io.Writer.
//
// The archive is marked clustered when tiles were added in increasing tile-ID order.
type Writer struct {
	mu   sync.Mutex
	f    *os.File      // final file (Create) or spool (NewWriter)
	buf  *bufio.Writer // buffered writes into f
	path string        // final path (Create mode only)
	base uint64        // file offset where tile data starts (dataStart for Create, 0 for spool)
	off  uint64        // bytes of tile data written

	entries  []Entry
	seen     map[[32]byte]Entry // content hash → stored bytes
	tiles    uint64
	inOrder  bool
	lastID   uint64
	hasLast  bool
	lastHash [32]byte
	done     bool
}

// dataStart reserves room for the header and a root directory of up to maxRootLen bytes in
// Create mode, so tile data can be written before the directories exist.
const dataStart = 16384

// maxRootLen keeps header + root directory within the first 16 KiB, as the spec requires
// for clients that fetch both in one request.
const maxRootLen = dataStart - HeaderLen

// Create starts an archive at path (written as path+".part" until Commit).
func Create(path string) (*Writer, error) {
	f, err := os.Create(path + ".part")
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(dataStart, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return newWriter(f, path, dataStart), nil
}

// NewWriter returns a writer spooling tile data into dir ("" = os.TempDir()); finish it
// with Finish.
func NewWriter(dir string) (*Writer, error) {
	f, err := os.CreateTemp(dir, "pmtiles-data-*")
	if err != nil {
		return nil, err
	}
	return newWriter(f, "", 0), nil
}

func newWriter(f *os.File, path string, base uint64) *Writer {
	return &Writer{f: f, buf: bufio.NewWriterSize(f, 1<<20), path: path, base: base,
		seen: map[[32]byte]Entry{}, inOrder: true}
}

// AddTile stores tile z/x/y (see Add).
func (w *Writer) AddTile(z uint8, x, y uint32, data []byte) error {
	return w.Add(ZxyToID(z, x, y), data)
}

// Add stores one tile; data must already be compressed per the header's TileCompression.
// Empty data is skipped (the tile stays absent). Adding the same ID twice is an error,
// reported here when tiles arrive in order and by Finish/Commit otherwise.
func (w *Writer) Add(id uint64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	h := sha256.Sum256(data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return errors.New("pmtiles: writer finished")
	}
	if w.hasLast && id <= w.lastID {
		if id == w.lastID {
			return fmt.Errorf("pmtiles: tile %d added twice", id)
		}
		w.inOrder = false
	}
	w.tiles++
	// Extend the previous run when the same content repeats at the next ID.
	if n := len(w.entries); n > 0 && w.hasLast && h == w.lastHash {
		last := &w.entries[n-1]
		if last.TileID+uint64(last.RunLength) == id {
			last.RunLength++
			w.lastID = id
			return nil
		}
	}
	w.lastID, w.hasLast, w.lastHash = id, true, h
	if prev, ok := w.seen[h]; ok {
		w.entries = append(w.entries, Entry{TileID: id, Offset: prev.Offset, Length: prev.Length, RunLength: 1})
		return nil
	}
	if _, err := w.buf.Write(data); err != nil {
		return err
	}
	e := Entry{TileID: id, Offset: w.off, Length: uint32(len(data)), RunLength: 1}
	w.off += uint64(len(data))
	w.seen[h] = e
	w.entries = append(w.entries, e)
	return nil
}

// Count returns the number of addressed tiles added so far.
func (w *Writer) Count() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tiles
}

// Abort discards the partial archive or spool.
func (w *Writer) Abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.abortLocked()
}

func (w *Writer) abortLocked() {
	if w.done {
		return
	}
	w.done = true
	_ = w.f.Close()
	_ = os.Remove(w.f.Name())
}

// prepare flushes data, sorts and merges entries, and builds the directories and metadata
// blob. It fills every computed header field except the section offsets.
func (w *Writer) prepare(h *Header, metadata []byte) (root, leaves, meta []byte, err error) {
	if w.done {
		return nil, nil, nil, errors.New("pmtiles: writer finished")
	}
	if err := w.buf.Flush(); err != nil {
		return nil, nil, nil, err
	}
	entries := w.entries
	if !w.inOrder {
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].TileID < entries[j].TileID })
		merged := entries[:0:0]
		for i, e := range entries {
			if i > 0 && e.TileID < entries[i-1].TileID+uint64(entries[i-1].RunLength) {
				return nil, nil, nil, fmt.Errorf("pmtiles: tile %d added twice", e.TileID)
			}
			if n := len(merged); n > 0 {
				last := &merged[n-1]
				if last.Offset == e.Offset && last.Length == e.Length && last.TileID+uint64(last.RunLength) == e.TileID {
					last.RunLength += e.RunLength
					continue
				}
			}
			merged = append(merged, e)
		}
		entries = merged
	}
	h.InternalCompression = CompressionGzip
	h.Clustered = w.inOrder
	if root, leaves, err = buildDirectories(entries, h.InternalCompression); err != nil {
		return nil, nil, nil, err
	}
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	if meta, err = Compress(metadata, h.InternalCompression); err != nil {
		return nil, nil, nil, err
	}
	h.AddressedTiles = w.tiles
	h.TileEntries = uint64(len(entries))
	h.TileContents = uint64(len(w.seen))
	h.DataLength = w.off
	h.RootLength = uint64(len(root))
	h.MetadataLength = uint64(len(meta))
	h.LeafLength = uint64(len(leaves))
	return root, leaves, meta, nil
}

// Finish streams the archive to out: header, root, metadata, leaf directories, tile data.
// Only for writers from NewWriter. h supplies tile type, compression, zooms, bounds and
// centre; offsets and counts are filled in. metadata is JSON (stored gzip-compressed).
func (w *Writer) Finish(out io.Writer, h Header, metadata []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path != "" {
		return errors.New("pmtiles: Finish on a Create writer; use Commit")
	}
	defer w.abortLocked() // removes the spool
	root, leaves, meta, err := w.prepare(&h, metadata)
	if err != nil {
		return err
	}
	h.RootOffset = HeaderLen
	h.MetadataOffset = h.RootOffset + h.RootLength
	h.LeafOffset = h.MetadataOffset + h.MetadataLength
	h.DataOffset = h.LeafOffset + h.LeafLength
	for _, b := range [][]byte{MarshalHeader(h), root, meta, leaves} {
		if _, err := out.Write(b); err != nil {
			return err
		}
	}
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(out, w.f)
	if err != nil {
		return err
	}
	if uint64(n) != w.off {
		return errors.New("pmtiles: short copy of tile data")
	}
	return nil
}

// Commit completes an archive started with Create and renames it into place. Layout:
// header, root (within the first 16 KiB), tile data, metadata, leaf directories.
func (w *Writer) Commit(h Header, metadata []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path == "" {
		return errors.New("pmtiles: Commit on a spooling writer; use Finish")
	}
	root, leaves, meta, err := w.prepare(&h, metadata)
	if err != nil {
		w.abortLocked()
		return err
	}
	h.RootOffset = HeaderLen
	h.DataOffset = w.base
	h.MetadataOffset = w.base + w.off
	h.LeafOffset = h.MetadataOffset + h.MetadataLength
	fail := func(err error) error { w.abortLocked(); return err }
	for _, b := range [][]byte{meta, leaves} {
		if _, err := w.buf.Write(b); err != nil {
			return fail(err)
		}
	}
	if err := w.buf.Flush(); err != nil {
		return fail(err)
	}
	if _, err := w.f.WriteAt(MarshalHeader(h), 0); err != nil {
		return fail(err)
	}
	if _, err := w.f.WriteAt(root, HeaderLen); err != nil {
		return fail(err)
	}
	if err := w.f.Sync(); err != nil {
		return fail(err)
	}
	w.done = true
	if err := w.f.Close(); err != nil {
		_ = os.Remove(w.f.Name())
		return err
	}
	return os.Rename(w.f.Name(), w.path)
}

func buildDirectories(entries []Entry, c uint8) (root, leaves []byte, err error) {
	root, err = MarshalDirectory(entries, c)
	if err != nil || len(root) <= maxRootLen {
		return root, nil, err
	}
	for leafSize := 4096; ; leafSize *= 2 {
		var rootEntries []Entry
		var lb []byte
		for i := 0; i < len(entries); i += leafSize {
			chunk := entries[i:min(i+leafSize, len(entries))]
			d, err := MarshalDirectory(chunk, c)
			if err != nil {
				return nil, nil, err
			}
			rootEntries = append(rootEntries, Entry{TileID: chunk[0].TileID, Offset: uint64(len(lb)), Length: uint32(len(d))})
			lb = append(lb, d...)
		}
		root, err = MarshalDirectory(rootEntries, c)
		if err != nil {
			return nil, nil, err
		}
		if len(root) <= maxRootLen {
			return root, lb, nil
		}
	}
}
