// Package web embeds the built admin UI (web/dist, produced by `mise run build:web`).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built UI rooted at dist/. It may hold only a placeholder index.html when
// the UI has not been built (see the build:web task).
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
