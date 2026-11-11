package cog

import "errors"

// TIFF-flavoured LZW (compression 5): codes are packed most significant bit
// first, start at 9 bits and grow to 12, and the width grows one code early
// ("early change"), as every modern TIFF writer does.

const (
	lzwClear   = 256
	lzwEOI     = 257
	lzwFirst   = 258
	lzwMaxCode = 4096
)

var errLZW = errors.New("cog: corrupt LZW stream")

// lzwDecode expands src into dst (whose length is the expected size) and
// returns the number of bytes written. Output beyond len(dst) is dropped.
func lzwDecode(dst, src []byte) (int, error) {
	var (
		prefix [lzwMaxCode]uint16
		suffix [lzwMaxCode]byte
		first  [lzwMaxCode]byte
		length [lzwMaxCode]uint16
	)
	for i := 0; i < 256; i++ {
		suffix[i], first[i], length[i] = byte(i), byte(i), 1
	}
	var (
		bits  uint32
		nbits uint
		pos   int
		width uint = 9
		next       = lzwFirst
		prev       = -1
		n     int
	)
	emit := func(code int) {
		l := int(length[code])
		end := n + l
		// Walk the chain backwards, writing from the end of the string.
		for i := end - 1; i >= n; i-- {
			if i < len(dst) {
				dst[i] = suffix[code]
			}
			code = int(prefix[code])
		}
		n = end
	}
	for n < len(dst) {
		for nbits < width {
			if pos >= len(src) {
				// Some writers omit the end-of-information code.
				return n, nil
			}
			bits = bits<<8 | uint32(src[pos])
			pos++
			nbits += 8
		}
		code := int(bits >> (nbits - width) & (1<<width - 1))
		nbits -= width
		switch {
		case code == lzwEOI:
			return n, nil
		case code == lzwClear:
			width, next, prev = 9, lzwFirst, -1
			continue
		case prev < 0:
			if code >= 256 {
				return n, errLZW
			}
			emit(code)
		case code < next:
			emit(code)
			prefix[next], suffix[next], first[next], length[next] = uint16(prev), first[code], first[prev], length[prev]+1
			next++
		case code == next:
			prefix[next], suffix[next], first[next], length[next] = uint16(prev), first[prev], first[prev], length[prev]+1
			next++
			emit(code)
		default:
			return n, errLZW
		}
		prev = code
		if next >= lzwMaxCode-1 {
			// The table is full; a well-formed stream sends Clear next.
			next = lzwMaxCode - 1
		}
		if next == 1<<width-1 && width < 12 {
			width++
		}
	}
	return n, nil
}
