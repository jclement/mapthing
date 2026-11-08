package elevation

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"math"
)

// TileSize is the edge of a terrain tile in pixels.
const TileSize = 256

// Terrarium encoding (the format MapLibre's raster-dem "terrarium" encoding and
// the Mapzen/AWS terrain tiles use): elevation = R*256 + G + B/256 - 32768.
// Resolution is 1/256 m; range -32768 .. 32767.996 m.

// EncodeTerrarium returns the RGB triplet for elevation e (metres). Values are
// rounded to the nearest 1/256 m and clamped to the representable range.
func EncodeTerrarium(e float64) (r, g, b uint8) {
	v := math.Round((e + 32768) * 256)
	if v < 0 {
		v = 0
	}
	if v > 0xFFFFFF {
		v = 0xFFFFFF
	}
	u := uint32(v)
	return uint8(u >> 16), uint8(u >> 8), uint8(u)
}

// DecodeTerrarium is the inverse of EncodeTerrarium.
func DecodeTerrarium(r, g, b uint8) float64 {
	return float64(r)*256 + float64(g) + float64(b)/256 - 32768
}

// Tile is one decoded terrain tile: elevations in row-major order (NaN where
// no source has data) and, for tiles at the archive's max zoom, the index of
// the source each pixel came from (0 = none; see Metadata.Sources).
type Tile struct {
	Elev []float32 // TileSize*TileSize
	Src  []uint8   // TileSize*TileSize, or nil when the tile carries no source chunk
}

// sourceChunk is a private, ancillary, safe-to-copy PNG chunk that carries the
// per-pixel source index (zlib-compressed bytes). Renderers ignore it.
const sourceChunk = "mtSr"

// EncodeTile writes a Terrarium PNG. NaN pixels are written as 0 m (sea level,
// so hillshading stays calm at coverage edges); src, if non-nil, is stored in a
// private chunk and marks those pixels with source 0 (no data).
func EncodeTile(elev []float32, src []uint8) ([]byte, error) {
	if len(elev) != TileSize*TileSize || (src != nil && len(src) != len(elev)) {
		return nil, errors.New("elevation: bad tile buffer size")
	}
	img := image.NewRGBA(image.Rect(0, 0, TileSize, TileSize))
	for i, e := range elev {
		v := float64(e)
		if math.IsNaN(v) {
			v = 0
		}
		r, g, b := EncodeTerrarium(v)
		p := img.Pix[i*4 : i*4+4 : i*4+4]
		p[0], p[1], p[2], p[3] = r, g, b, 255
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if src == nil {
		return out, nil
	}
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	zw.Write(src)
	zw.Close()
	// Insert the chunk just before IEND (the last 12 bytes).
	iend := len(out) - 12
	var chunk bytes.Buffer
	binary.Write(&chunk, binary.BigEndian, uint32(z.Len()))
	chunk.WriteString(sourceChunk)
	chunk.Write(z.Bytes())
	crc := crc32.NewIEEE()
	crc.Write([]byte(sourceChunk))
	crc.Write(z.Bytes())
	binary.Write(&chunk, binary.BigEndian, crc.Sum32())
	res := make([]byte, 0, len(out)+chunk.Len())
	res = append(res, out[:iend]...)
	res = append(res, chunk.Bytes()...)
	res = append(res, out[iend:]...)
	return res, nil
}

// DecodeTile decodes a Terrarium PNG written by EncodeTile (or any Terrarium
// PNG). Pixels whose source index is 0 decode as NaN.
func DecodeTile(data []byte) (*Tile, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() != TileSize || b.Dy() != TileSize {
		return nil, fmt.Errorf("elevation: tile is %dx%d, want %d", b.Dx(), b.Dy(), TileSize)
	}
	t := &Tile{Elev: make([]float32, TileSize*TileSize)}
	switch m := img.(type) {
	case *image.RGBA:
		for i := range t.Elev {
			p := m.Pix[i*4:]
			t.Elev[i] = float32(DecodeTerrarium(p[0], p[1], p[2]))
		}
	case *image.NRGBA:
		for i := range t.Elev {
			p := m.Pix[i*4:]
			t.Elev[i] = float32(DecodeTerrarium(p[0], p[1], p[2]))
		}
	default:
		for y := 0; y < TileSize; y++ {
			for x := 0; x < TileSize; x++ {
				r, g, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				t.Elev[y*TileSize+x] = float32(DecodeTerrarium(uint8(r>>8), uint8(g>>8), uint8(bb>>8)))
			}
		}
	}
	src, err := readSourceChunk(data)
	if err != nil {
		return nil, err
	}
	if src != nil {
		t.Src = src
		for i, s := range src {
			if s == 0 {
				t.Elev[i] = float32(math.NaN())
			}
		}
	}
	return t, nil
}

func readSourceChunk(data []byte) ([]uint8, error) {
	if len(data) < 8 {
		return nil, errors.New("elevation: short png")
	}
	p := data[8:]
	for len(p) >= 12 {
		n := binary.BigEndian.Uint32(p)
		if uint64(n)+12 > uint64(len(p)) {
			return nil, errors.New("elevation: truncated png chunk")
		}
		typ := string(p[4:8])
		if typ == sourceChunk {
			zr, err := zlib.NewReader(bytes.NewReader(p[8 : 8+n]))
			if err != nil {
				return nil, err
			}
			src, err := io.ReadAll(io.LimitReader(zr, TileSize*TileSize+1))
			if err != nil {
				return nil, err
			}
			if len(src) != TileSize*TileSize {
				return nil, errors.New("elevation: bad source chunk size")
			}
			return src, nil
		}
		if typ == "IEND" {
			break
		}
		p = p[12+n:]
	}
	return nil, nil
}
