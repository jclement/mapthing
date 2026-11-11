package tiles

import (
	"testing"
	"testing/fstest"
)

// A raster tile's cache key carries the style's revision: after an update that changes a
// style, tiles drawn with the old one are not served from the cache.
func TestStyleRevFollowsTheStyle(t *testing.T) {
	mk := func(body string) *Server {
		s, err := New(Options{DataDir: t.TempDir(), Assets: fstest.MapFS{
			"styles/a.json": {Data: []byte(body)},
			"styles/b.json": {Data: []byte(`{"version":8,"layers":[]}`)},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	v1, v1again, v2 := mk(`{"version":8,"layers":[],"n":1}`), mk(`{"version":8,"layers":[],"n":1}`), mk(`{"version":8,"layers":[],"n":2}`)
	if v1.styleRev("a") != v1again.styleRev("a") {
		t.Error("the same style gave two revisions: every restart would empty the cache")
	}
	if v1.styleRev("a") == v2.styleRev("a") {
		t.Error("a changed style kept its revision: old tiles would be served")
	}
	if v1.styleRev("b") != v2.styleRev("b") {
		t.Error("an unchanged style changed revision")
	}
	if rev := v1.styleRev("a"); len(rev) != 9 || rev[0] != 's' {
		t.Errorf("revision %q", rev)
	}
}
