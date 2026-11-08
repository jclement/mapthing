package pmtiles

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Reader reads tiles from an archive. It is safe for concurrent use.
type Reader struct {
	r      io.ReaderAt
	closer io.Closer
	Header Header
	root   []Entry

	mu     sync.Mutex
	leaves map[uint64][]Entry // by leaf offset; bounded, see getLeaf
}

const maxCachedLeaves = 64

// Open opens an archive file.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	rd, err := NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rd.closer = f
	return rd, nil
}

// NewReader reads the header and root directory from r.
func NewReader(r io.ReaderAt) (*Reader, error) {
	buf := make([]byte, headerLen)
	if _, err := r.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("pmtiles: header: %w", err)
	}
	h, err := unmarshalHeader(buf)
	if err != nil {
		return nil, err
	}
	rootBytes := make([]byte, h.RootLength)
	if _, err := r.ReadAt(rootBytes, int64(h.RootOffset)); err != nil {
		return nil, fmt.Errorf("pmtiles: root directory: %w", err)
	}
	root, err := deserializeEntries(rootBytes, h.InternalCompression)
	if err != nil {
		return nil, err
	}
	return &Reader{r: r, Header: h, root: root, leaves: map[uint64][]Entry{}}, nil
}

// Close closes the underlying file, if Open created it.
func (rd *Reader) Close() error {
	if rd.closer != nil {
		return rd.closer.Close()
	}
	return nil
}

// Metadata unmarshals the archive's JSON metadata into v.
func (rd *Reader) Metadata(v any) error {
	b := make([]byte, rd.Header.MetadataLength)
	if _, err := rd.r.ReadAt(b, int64(rd.Header.MetadataOffset)); err != nil {
		return err
	}
	raw, err := decompress(b, rd.Header.InternalCompression)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// Tile returns the bytes of tile z/x/y (still in the archive's tile
// compression), or ok=false if the archive does not contain it.
func (rd *Reader) Tile(z uint8, x, y uint32) (data []byte, ok bool, err error) {
	id := ZxyToID(z, x, y)
	dir := rd.root
	for depth := 0; depth < 4; depth++ {
		e, found := findEntry(dir, id)
		if !found {
			return nil, false, nil
		}
		if e.RunLength > 0 {
			data = make([]byte, e.Length)
			if _, err := rd.r.ReadAt(data, int64(rd.Header.DataOffset+e.Offset)); err != nil {
				return nil, false, err
			}
			return data, true, nil
		}
		dir, err = rd.getLeaf(e)
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("pmtiles: directory too deep")
}

func (rd *Reader) getLeaf(e Entry) ([]Entry, error) {
	rd.mu.Lock()
	if l, ok := rd.leaves[e.Offset]; ok {
		rd.mu.Unlock()
		return l, nil
	}
	rd.mu.Unlock()
	b := make([]byte, e.Length)
	if _, err := rd.r.ReadAt(b, int64(rd.Header.LeafOffset+e.Offset)); err != nil {
		return nil, err
	}
	l, err := deserializeEntries(b, rd.Header.InternalCompression)
	if err != nil {
		return nil, err
	}
	rd.mu.Lock()
	if len(rd.leaves) >= maxCachedLeaves {
		for k := range rd.leaves { // drop an arbitrary leaf
			delete(rd.leaves, k)
			break
		}
	}
	rd.leaves[e.Offset] = l
	rd.mu.Unlock()
	return l, nil
}
