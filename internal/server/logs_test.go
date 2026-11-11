package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/jobs"
	"github.com/jclement/mapthing/internal/logs"
)

func newLogEnv(t *testing.T) (*env, *logs.Sink) {
	t.Helper()
	var sink *logs.Sink
	e := newEnv(t, false, func(d *Deps) {
		sink = logs.New(logs.Options{Dir: filepath.Join(d.DataDir, "logs")})
		t.Cleanup(sink.Close)
		d.Logs = sink
		d.Jobs.Log = func(id int64, _ string, at time.Time, line string) {
			sink.Add(logs.Entry{Time: at, Source: logs.SourceBuild, Job: "7", Msg: line})
		}
	})
	return e, sink
}

type logsResp struct {
	Entries []logs.Entry `json:"entries"`
	More    bool         `json:"more"`
	Sources []string     `json:"sources"`
	Store   logs.Status  `json:"store"`
}

func TestLogsAPI(t *testing.T) {
	e, sink := newLogEnv(t)
	if r, _ := e.do(nil, "GET", "/admin/api/logs", nil, nil); r.StatusCode != 401 {
		t.Fatalf("logs without a session: %d", r.StatusCode)
	}
	c := e.login()
	const key = "mt_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	sink.Log(logs.SourceServer, logs.LevelInfo, "", "listening addr=:8080")
	sink.Log("valhalla@8002", "", "", "2026/11/11 10:00:00 [WARN] slow request /route?key="+key)
	sink.Log("renderer", "", "", "Error: fetch failed, headers {\"X-Goog-Api-Key\":\"AIzaSyA-1234567890abcdefghijklmnopqrstuv\",\"Authorization\":\"Bearer "+key+"\"}")
	sink.Log(logs.SourceBuild, logs.LevelInfo, "7", "== stage download")

	get := func(q string) logsResp {
		t.Helper()
		r, b := e.do(c, "GET", "/admin/api/logs"+q, nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("GET logs%s: %d %s", q, r.StatusCode, b)
		}
		if strings.Contains(string(b), key[3:]) || strings.Contains(string(b), "AIzaSy") {
			t.Fatalf("a key reached the API: %s", b)
		}
		var out logsResp
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	all := get("")
	if len(all.Entries) != 4 || all.More || len(all.Sources) != 4 || all.Sources[0] != "mapthing" {
		t.Fatalf("all: %+v", all)
	}
	if all.Store.Oldest == nil || all.Store.MaxBytes == 0 {
		t.Errorf("store status %+v", all.Store)
	}
	if got := get("?source=renderer,valhalla@8002&level=warn"); len(got.Entries) != 2 {
		t.Errorf("source+level: %+v", got.Entries)
	}
	if got := get("?level=error"); len(got.Entries) != 1 || got.Entries[0].Source != "renderer" {
		t.Errorf("level=error: %+v", got.Entries)
	}
	if got := get("?job=7"); len(got.Entries) != 1 || got.Entries[0].Msg != "== stage download" {
		t.Errorf("job: %+v", got.Entries)
	}
	if got := get("?q=SLOW"); len(got.Entries) != 1 {
		t.Errorf("text: %+v", got.Entries)
	}
	if got := get("?limit=2"); len(got.Entries) != 2 || !got.More || got.Entries[1].Msg != "== stage download" {
		t.Errorf("limit: %+v", got)
	}
	page := get("?limit=2")
	if got := get("?before=" + jsonID(page.Entries[0].ID)); len(got.Entries) != 2 || got.Entries[0].Msg != "listening addr=:8080" {
		t.Errorf("before: %+v", got.Entries)
	}
	if got := get("?since=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); len(got.Entries) != 0 {
		t.Errorf("since in the future: %+v", got.Entries)
	}
	for _, bad := range []string{"?level=loud", "?since=yesterday", "?before=x"} {
		if r, _ := e.do(c, "GET", "/admin/api/logs"+bad, nil, nil); r.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", bad, r.StatusCode)
		}
	}

	// Download: the filtered view, as text and as JSON lines.
	r, b := e.do(c, "GET", "/admin/api/logs/download?level=warn", nil, nil)
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %v", r.StatusCode, r.Header)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "WARN  [valhalla@8002]") || strings.Contains(string(b), key[3:]) || strings.Contains(string(b), "AIzaSy") {
		t.Errorf("download text:\n%s", b)
	}
	_, b = e.do(c, "GET", "/admin/api/logs/download?format=jsonl&job=7", nil, nil)
	var one logs.Entry
	if err := json.Unmarshal(b, &one); err != nil || one.Job != "7" {
		t.Errorf("download jsonl: %s (%v)", b, err)
	}
}

func jsonID(id int64) string {
	b, _ := json.Marshal(struct {
		ID int64 `json:",string"`
	}{id})
	return strings.Trim(strings.TrimSuffix(strings.TrimPrefix(string(b), `{"ID":`), "}"), `"`)
}

func TestLogsStream(t *testing.T) {
	e, sink := newLogEnv(t)
	c := e.login()
	sink.Log("renderer", logs.LevelInfo, "", "before one")
	sink.Log("renderer", logs.LevelInfo, "", "before two")
	hist, _ := sink.Query(logs.Filter{}, 10)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/admin/api/logs/stream?source=renderer&after="+jsonID(hist[0].ID), nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	got := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var en logs.Entry
				if json.Unmarshal([]byte(d), &en) == nil {
					got <- en.Msg
				}
			}
		}
	}()
	next := func() string {
		t.Helper()
		select {
		case m := <-got:
			return m
		case <-ctx.Done():
			t.Fatal("stream: timed out")
			return ""
		}
	}
	if m := next(); m != "before two" {
		t.Fatalf("catch-up: got %q, want the entry after the given id", m)
	}
	sink.Log("valhalla@8002", logs.LevelInfo, "", "filtered out")
	sink.Log("renderer", logs.LevelInfo, "", "live ?key=verysecretvalue123")
	if m := next(); m != "live ?key="+logs.Mask {
		t.Fatalf("live: got %q", m)
	}
}

// A build's output reaches the log with its job id (the Runner.Log hook).
func TestJobOutputReachesLogs(t *testing.T) {
	e, sink := newLogEnv(t)
	e.jobs.Register("build", func(_ context.Context, j *jobs.Job) error {
		j.Logf("$ planetiler --download-url=https://example.com/x?token=tok-123456789")
		return nil
	})
	c := e.login()
	if r, b := e.do(c, "POST", "/admin/api/build", map[string]any{}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 202 {
		t.Fatalf("build: %d %s", r.StatusCode, b)
	}
	waitFor(t, func() bool {
		got, _ := sink.Query(logs.Filter{Sources: []string{logs.SourceBuild}, Text: "planetiler"}, 5)
		return len(got) == 1 && got[0].Job == "7" && !strings.Contains(got[0].Msg, "tok-123456789")
	})
}
