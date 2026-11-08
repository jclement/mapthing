package pmtiles

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// Writer builds a clustered PMTiles archive. Tiles must be added in increasing TileID order.
// Tile bytes are spooled to a temporary file, so memory use is bounded by the directory.
type Writer struct {
	tmp      *os.File
	buf      *bufio.Writer
	off      uint64
	entries  []Entry
	seen     map[[32]byte]Entry // content hash → first entry (for deduplication)
	lastID   uint64
	hasLast  bool
	lastHash [32]byte
	tiles    uint64
}

// NewWriter returns a writer spooling into dir ("" = os.TempDir()).
func NewWriter(dir string) (*Writer, error) {
	f, err := os.CreateTemp(dir, "pmtiles-data-*")
	if err != nil {
		return nil, err
	}
	return &Writer{tmp: f, buf: bufio.NewWriterSize(f, 1<<20), seen: map[[32]byte]Entry{}}, nil
}

// Add appends a tile. data must already be compressed per the header's TileCompression.
func (w *Writer) Add(id uint64, data []byte) error {
	if w.hasLast && id <= w.lastID {
		return fmt.Errorf("tile %d added out of order (after %d)", id, w.lastID)
	}
	if len(data) == 0 {
		return nil
	}
	h := sha256.Sum256(data)
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
func (w *Writer) Count() uint64 { return w.tiles }

// Abort discards the spool file.
func (w *Writer) Abort() {
	_ = w.tmp.Close()
	_ = os.Remove(w.tmp.Name())
}

// Finish writes the archive to out. h supplies tile type, compression, zooms, bounds and
// centre; offsets and counts are filled in. metadata is JSON (compressed with gzip).
func (w *Writer) Finish(out io.Writer, h Header, metadata []byte) error {
	defer w.Abort()
	if err := w.buf.Flush(); err != nil {
		return err
	}
	h.InternalCompression = CompressionGzip
	h.Clustered = true
	root, leaves, err := buildDirectories(w.entries, h.InternalCompression)
	if err != nil {
		return err
	}
	meta, err := Compress(metadata, h.InternalCompression)
	if err != nil {
		return err
	}
	h.RootOffset = HeaderLen
	h.RootLength = uint64(len(root))
	h.MetadataOffset = h.RootOffset + h.RootLength
	h.MetadataLength = uint64(len(meta))
	h.LeafOffset = h.MetadataOffset + h.MetadataLength
	h.LeafLength = uint64(len(leaves))
	h.DataOffset = h.LeafOffset + h.LeafLength
	h.DataLength = w.off
	h.AddressedTiles = w.tiles
	h.TileEntries = uint64(len(w.entries))
	h.TileContents = uint64(len(w.seen))
	for _, b := range [][]byte{MarshalHeader(h), root, meta, leaves} {
		if _, err := out.Write(b); err != nil {
			return err
		}
	}
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(out, w.tmp)
	if err != nil {
		return err
	}
	if uint64(n) != w.off {
		return errors.New("short copy of tile data")
	}
	return nil
}

// maxRootLen keeps header + root directory within the first 16 KiB, as the spec recommends,
// so a client can fetch both in one request.
const maxRootLen = 16384 - HeaderLen

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
