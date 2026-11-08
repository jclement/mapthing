package routing

import (
	"net/http"
	"testing"
)

// The server registers "GET /" for the admin UI; Mount must not panic beside it.
func TestMountBesideRootRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) {})
	NewHandlers(New("http://127.0.0.1:1")).Mount(mux, nil)
}
