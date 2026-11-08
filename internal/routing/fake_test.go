package routing

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fakeValhalla serves recorded fixtures. Each handler gets the decoded request body and
// returns a status and a testdata file name (or raw JSON when it starts with "{" or "[").
type fakeValhalla struct {
	t        *testing.T
	mu       sync.Mutex
	handlers map[string]func(body map[string]any) (int, string)
	bodies   map[string][]map[string]any
}

func newFake(t *testing.T) (*fakeValhalla, *Client) {
	t.Helper()
	f := &fakeValhalla{t: t, handlers: map[string]func(map[string]any) (int, string){}, bodies: map[string][]map[string]any{}}
	// Default locate: every location snaps to itself, except points at sea (lon > -75).
	f.handlers["locate"] = func(body map[string]any) (int, string) {
		var out []map[string]any
		for _, l := range body["locations"].([]any) {
			loc := l.(map[string]any)
			lat, lon := loc["lat"].(float64), loc["lon"].(float64)
			r := map[string]any{"input_lat": lat, "input_lon": lon, "edges": []any{}, "nodes": []any{}}
			if lon < -75 {
				r["edges"] = []any{map[string]any{
					"correlated_lat": lat + 0.0001, "correlated_lon": lon, "distance": 11.1,
					"edge_info": map[string]any{"names": []string{"Test Road"}},
				}}
			}
			out = append(out, r)
		}
		b, _ := json.Marshal(out)
		return 200, string(b)
	}
	f.handlers["trace_attributes"] = func(map[string]any) (int, string) { return 200, "valhalla_trace_track.json" }
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, New(srv.URL)
}

func (f *fakeValhalla) serve(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/")
	var body map[string]any
	data, _ := io.ReadAll(r.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &body); err != nil {
			f.t.Errorf("fake valhalla: bad JSON for %s: %v", action, err)
		}
	}
	f.mu.Lock()
	f.bodies[action] = append(f.bodies[action], body)
	h := f.handlers[action]
	f.mu.Unlock()
	if h == nil {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error_code":106,"error":"Try any of","status_code":404}`))
		return
	}
	status, src := h(body)
	out := []byte(src)
	if !strings.HasPrefix(src, "{") && !strings.HasPrefix(src, "[") {
		var err error
		if out, err = os.ReadFile(filepath.Join("testdata", src)); err != nil {
			f.t.Fatalf("fixture: %v", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func (f *fakeValhalla) on(action string, status int, src string) {
	f.handlers[action] = func(map[string]any) (int, string) { return status, src }
}

func (f *fakeValhalla) last(action string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.bodies[action]
	if len(b) == 0 {
		f.t.Fatalf("no %s request made", action)
	}
	return b[len(b)-1]
}

// golden compares v (as indented JSON, with volatile fields removed) to testdata/golden/name.
func golden(t *testing.T, name string, data []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, data)
	}
	stripVolatile(v)
	got, _ := json.MarshalIndent(v, "", "  ")
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s missing (run go test -update): %v", name, err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs from golden; run `go test ./internal/routing -update` and review the diff.\ngot:\n%s", name, truncate(string(got), 3000))
	}
}

func stripVolatile(v any) {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "timestamp")
		for _, c := range x {
			stripVolatile(c)
		}
	case []any:
		for _, c := range x {
			stripVolatile(c)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// do runs one request through the mounted handlers.
func do(t *testing.T, c *Client, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	NewHandlers(c).Mount(mux, nil)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
