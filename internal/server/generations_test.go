package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type gensResp struct {
	Generations []struct {
		ID          string   `json:"id"`
		Status      string   `json:"status"`
		Owned       bool     `json:"owned"`
		Problems    []string `json:"problems"`
		Bytes       int64    `json:"bytes"`
		CanActivate bool     `json:"can_activate"`
		CanDelete   bool     `json:"can_delete"`
	} `json:"generations"`
	Disk struct {
		Bytes    int64 `json:"bytes"`
		SumBytes int64 `json:"sum_bytes"`
	} `json:"disk"`
}

func TestGenerationsListAndDelete(t *testing.T) {
	var deleted []string
	e := newEnv(t, true, func(d *Deps) {
		for _, id := range []string{"g0", "g2", "g3", "g4"} {
			dir := filepath.Join(d.DataDir, "generations", id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"generation":"`+id+`","ok":true,"created_at":"2026-11-0`+id[1:]+`T00:00:00Z"}`), 0o644)
		}
		d.GenerationInventory = func() ([]GenerationState, int64) {
			return []GenerationState{
				{ID: "g1", Status: "live", Owned: true, Valid: true},
				{ID: "g0", Status: "previous", Owned: true, Valid: true},
				{ID: "g2", Status: "incoming", Valid: true},
				{ID: "g3", Status: "invalid", Owned: true, Problems: []string{"basemap.pmtiles is missing"}},
				{ID: "g4", Status: "building"},
			}, 1234
		}
		d.ActivateGeneration = func(context.Context, string) (Activation, error) { return Activation{}, nil }
		d.DeleteGeneration = func(_ context.Context, id string) error {
			switch id {
			case "g1":
				return &ActivateError{Status: http.StatusConflict, Code: "live", Message: "generation g1 is the one being served"}
			case "g4":
				return &ActivateError{Status: http.StatusConflict, Code: "busy", Message: "being built"}
			case "nope":
				return &ActivateError{Status: http.StatusNotFound, Code: "no_generation", Message: "no such generation"}
			}
			deleted = append(deleted, id)
			return os.RemoveAll(filepath.Join(d.DataDir, "generations", id))
		}
	})
	c := e.login()
	list := func() map[string]int {
		t.Helper()
		r, b := e.do(c, "GET", "/admin/api/generations", nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("generations: %d %s", r.StatusCode, b)
		}
		var out gensResp
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		if out.Disk.Bytes != 1234 {
			t.Errorf("disk bytes %d, want the inventory's (shared files counted once)", out.Disk.Bytes)
		}
		idx := map[string]int{}
		for i, g := range out.Generations {
			idx[g.ID] = i
			want := map[string][3]any{ // status, can_activate, can_delete
				"g1": {"live", false, false}, "g0": {"previous", true, true}, "g2": {"incoming", true, true},
				"g3": {"invalid", false, true}, "g4": {"building", false, false},
			}[g.ID]
			if g.Status != want[0] || g.CanActivate != want[1] || g.CanDelete != want[2] {
				t.Errorf("%s: status=%s can_activate=%v can_delete=%v, want %v", g.ID, g.Status, g.CanActivate, g.CanDelete, want)
			}
			if g.ID == "g3" && len(g.Problems) != 1 {
				t.Errorf("g3 problems %v", g.Problems)
			}
			if g.ID == "g2" && g.Owned {
				t.Error("g2 is an incoming copy: not owned")
			}
		}
		return idx
	}
	if n := len(list()); n != 5 {
		t.Fatalf("listed %d generations, want 5", n)
	}

	origin := map[string]string{"Origin": e.ts.URL}
	for id, want := range map[string]struct {
		status int
		code   string
	}{"g1": {409, "live"}, "g4": {409, "busy"}, "nope": {404, "no_generation"}} {
		r, b := e.do(c, "DELETE", "/admin/api/generations/"+id, nil, origin)
		var er struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(b, &er)
		if r.StatusCode != want.status || er.Error.Code != want.code {
			t.Errorf("DELETE %s: %d %s, want %d %s", id, r.StatusCode, b, want.status, want.code)
		}
	}
	if r, _ := e.do(nil, "DELETE", "/admin/api/generations/g0", nil, origin); r.StatusCode != 401 {
		t.Errorf("delete without a session: %d", r.StatusCode)
	}
	if r, _ := e.do(c, "DELETE", "/admin/api/generations/g0", nil, map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Errorf("cross-origin delete: %d", r.StatusCode)
	}
	if len(deleted) != 0 {
		t.Fatalf("refused deletes reached the pipeline: %v", deleted)
	}
	if r, b := e.do(c, "DELETE", "/admin/api/generations/g0", nil, origin); r.StatusCode != 204 {
		t.Fatalf("DELETE g0: %d %s", r.StatusCode, b)
	}
	if _, still := list()["g0"]; still || len(deleted) != 1 {
		t.Errorf("g0 still listed after delete (deleted=%v)", deleted)
	}
	// The delete is in the history.
	_, b := e.do(c, "GET", "/admin/api/history?kind=generation", nil, nil)
	if !json.Valid(b) || !strings.Contains(string(b), "generation g0 deleted by hand") {
		t.Errorf("history: %s", b)
	}
}
