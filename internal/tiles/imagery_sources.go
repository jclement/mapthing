package tiles

// This file is owned by the style worker: it chooses the open imagery upstream per tile.
// The stub below configures no source, so /tiles/imagery answers 204 (empty tile).

// ImageryUpstream returns the upstream URL for imagery tile z/x/y, or ok=false when no source
// covers it.
func ImageryUpstream(z, x, y int) (url string, ok bool) {
	_, _, _ = z, x, y
	return "", false
}

// ImageryAttribution is the attribution HTML for the imagery source.
func ImageryAttribution() string { return "" }
