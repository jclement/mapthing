// Package assets embeds the GL styles, glyphs and sprites served by mapthing. The style
// worker fills styles/ (*.json with {{BASE_URL}} and {{KEY_QS}} placeholders), fonts/
// (<fontstack>/<range>.pbf) and sprites/ (<name>[@2x].{json,png}).
package assets

import (
	"embed"
	"io/fs"
)

//go:embed all:styles all:fonts all:sprites
var files embed.FS

// FS returns the embedded assets (paths styles/…, fonts/…, sprites/…).
func FS() fs.FS { return files }
