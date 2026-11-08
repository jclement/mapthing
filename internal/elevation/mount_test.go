package elevation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jclement/mapthing/internal/server"
)

// The handler must satisfy core's Mountable.
var _ server.Mountable = (*Handler)(nil)

func TestMountWrapsEveryRoute(t *testing.T) {
	mux := http.NewServeMux()
	var kinds []string
	NewHandler(NewTerrain("/nonexistent/terrain.pmtiles", 0)).Mount(mux, func(kind string, h http.Handler) http.Handler {
		kinds = append(kinds, kind)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	})
	if len(kinds) != 5 {
		t.Fatalf("wrapped %d routes, want 5", len(kinds))
	}
	for _, k := range kinds {
		if k != "elevation" {
			t.Errorf("kind %q", k)
		}
	}
	for _, u := range []string{"/api/v1/elevation?points=0,0", "/maps/api/elevation/json?locations=0,0", "/api/v1/elevation/sources"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", u, nil))
		if rr.Code != http.StatusTeapot {
			t.Errorf("%s not wrapped (code %d)", u, rr.Code)
		}
	}
}
