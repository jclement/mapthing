package server

import "net/http"

// Wrap is the middleware core applies to every keyed endpoint: API-key or admin-session auth,
// per-key limits and usage counting under kind (tile, raster, route, elevation, pack, compat).
type Wrap func(kind string, h http.Handler) http.Handler

// Mountable is implemented by feature packages (routing, elevation) that register their own
// HTTP paths. Mount must register every keyed handler as mux.Handle(pattern, wrap(kind, h)).
// The method signature uses only standard types, so implementers need not import this package.
type Mountable interface {
	Mount(mux *http.ServeMux, wrap func(kind string, h http.Handler) http.Handler)
}
