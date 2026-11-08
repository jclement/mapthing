package pmtiles

// ZxyToID converts a tile address to a PMTiles tile ID: the count of all tiles at lower zooms
// plus the position of (x, y) along the Hilbert curve at zoom z.
func ZxyToID(z uint8, x, y uint32) uint64 {
	var base uint64
	for i := uint8(0); i < z; i++ {
		base += uint64(1) << (2 * i)
	}
	n := uint64(1) << z
	tx, ty := uint64(x), uint64(y)
	var d uint64
	for s := n / 2; s > 0; s /= 2 {
		var rx, ry uint64
		if tx&s > 0 {
			rx = 1
		}
		if ty&s > 0 {
			ry = 1
		}
		d += s * s * ((3 * rx) ^ ry)
		tx, ty = hilbertRotate(n, tx, ty, rx, ry)
	}
	return base + d
}

// IDToZxy converts a PMTiles tile ID back to z/x/y.
func IDToZxy(id uint64) (uint8, uint32, uint32) {
	var base uint64
	var z uint8
	for {
		count := uint64(1) << (2 * z)
		if id < base+count {
			break
		}
		base += count
		z++
	}
	n := uint64(1) << z
	t := id - base
	var x, y uint64
	for s := uint64(1); s < n; s *= 2 {
		rx := 1 & (t / 2)
		ry := 1 & (t ^ rx)
		x, y = hilbertRotate(s, x, y, rx, ry)
		x += s * rx
		y += s * ry
		t /= 4
	}
	return z, uint32(x), uint32(y)
}

func hilbertRotate(n, x, y, rx, ry uint64) (uint64, uint64) {
	if ry == 0 {
		if rx == 1 {
			x = n - 1 - x
			y = n - 1 - y
		}
		x, y = y, x
	}
	return x, y
}
