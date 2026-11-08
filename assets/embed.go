// Package assets embeds the GL styles, glyphs and sprites served by mapthing. The style
// worker fills styles/ (*.json with {{BASE_URL}} and {{KEY_QS}} placeholders), fonts/
// (<fontstack>/<range>.pbf) and sprites/ (<name>[@2x].{json,png}).
package assets

import (
	"embed"
	"io/fs"
)

// Only the generated styles (styles/*.json); the generator's sources, package files and
// node_modules stay out. Non-style JSON (package.json) is filtered by the tiles package.
//
//go:embed styles/*.json all:fonts all:sprites
var files embed.FS

// FS returns the embedded assets (paths styles/…, fonts/…, sprites/…).
func FS() fs.FS { return files }
