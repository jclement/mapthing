package pmtiles

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Reader reads tiles from a PMTiles archive.
type Reader struct {
	ra     io.ReaderAt
	closer io.Closer
	size   int64
	Header Header
	root   []Entry

	mu     sync.Mutex
	leaves map[uint64][]Entry // leaf directory cache by offset
}

const maxLeafCache = 256

// Open opens an archive file.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r, err := NewReader(f, fi.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.closer = f
	return r, nil
}

// NewReader reads an archive from ra of the given size.
func NewReader(ra io.ReaderAt, size int64) (*Reader, error) {
	hb := make([]byte, HeaderLen)
	if _, err := ra.ReadAt(hb, 0); err != nil {
		return nil, err
	}
	h, err := UnmarshalHeader(hb)
	if err != nil {
		return nil, err
	}
	r := &Reader{ra: ra, size: size, Header: h, leaves: map[uint64][]Entry{}}
	rb, err := r.read(h.RootOffset, h.RootLength)
	if err != nil {
		return nil, fmt.Errorf("root directory: %w", err)
	}
	if r.root, err = UnmarshalDirectory(rb, h.InternalCompression); err != nil {
		return nil, fmt.Errorf("root directory: %w", err)
	}
	return r, nil
}

// Size returns the archive size in bytes.
func (r *Reader) Size() int64 { return r.size }

// ReaderAt exposes the underlying reader (for serving the whole archive).
func (r *Reader) ReaderAt() io.ReaderAt { return r.ra }

// Close closes the underlying file.
func (r *Reader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

func (r *Reader) read(off, n uint64) ([]byte, error) {
	if off+n > uint64(r.size) || n > 1<<30 {
		return nil, errors.New("range outside archive")
	}
	b := make([]byte, n)
	if _, err := r.ra.ReadAt(b, int64(off)); err != nil && (!errors.Is(err, io.EOF) || uint64(len(b)) != n) {
		return nil, err
	}
	return b, nil
}

// Metadata returns the decompressed metadata JSON.
func (r *Reader) Metadata() ([]byte, error) {
	if r.Header.MetadataLength == 0 {
		return []byte("{}"), nil
	}
	b, err := r.read(r.Header.MetadataOffset, r.Header.MetadataLength)
	if err != nil {
		return nil, err
	}
	return Decompress(b, r.Header.InternalCompression)
}

// Get returns the tile at z/x/y as stored (compressed per Header.TileCompression), or ok=false.
func (r *Reader) Get(z uint8, x, y uint32) ([]byte, bool, error) {
	if z > 31 || x >= 1<<z || y >= 1<<z {
		return nil, false, nil
	}
	return r.GetID(ZxyToID(z, x, y))
}

// GetID returns the tile with the given ID.
func (r *Reader) GetID(id uint64) ([]byte, bool, error) {
	dir := r.root
	for depth := 0; depth < 4; depth++ {
		e, ok := findEntry(dir, id)
		if !ok {
			return nil, false, nil
		}
		if e.RunLength > 0 {
			b, err := r.read(r.Header.DataOffset+e.Offset, uint64(e.Length))
			return b, err == nil, err
		}
		leaf, err := r.leaf(e.Offset, uint64(e.Length))
		if err != nil {
			return nil, false, err
		}
		dir = leaf
	}
	return nil, false, errors.New("directory nesting too deep")
}

func (r *Reader) leaf(off, n uint64) ([]Entry, error) {
	r.mu.Lock()
	es, ok := r.leaves[off]
	r.mu.Unlock()
	if ok {
		return es, nil
	}
	b, err := r.read(r.Header.LeafOffset+off, n)
	if err != nil {
		return nil, err
	}
	if es, err = UnmarshalDirectory(b, r.Header.InternalCompression); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if len(r.leaves) >= maxLeafCache {
		for k := range r.leaves {
			delete(r.leaves, k)
			if len(r.leaves) < maxLeafCache/2 {
				break
			}
		}
	}
	r.leaves[off] = es
	r.mu.Unlock()
	return es, nil
}
