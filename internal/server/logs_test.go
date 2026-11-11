package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

// Review 1, finding 3: what a build prints is masked before it is stored in the job log,
// published on the event stream, kept as the job's error or written to history; and the admin
// API masks it again on the way out, for rows stored before that was so. Synthetic secrets.
func TestJobOutputIsRedactedEverywhere(t *testing.T) {
	const goog = "AIzaSyA-1234567890abcdefghijklmnopqrstuv"
	const urlPass = "hunter2hunter2"
	const adminLiteral = "open-sesame-registered-1"
	secrets := []string{goog, urlPass, adminLiteral, "tailsecret99", "progresstoken77"}
	var sink *logs.Sink
	e := newEnv(t, false, func(d *Deps) {
		sink = logs.New(logs.Options{})
		sink.AddSecret(adminLiteral)
		d.Logs = sink
		d.Jobs.Redact = sink.Redact
		d.Jobs.Log = func(id int64, _ string, at time.Time, line string) {
			sink.Add(logs.Entry{Time: at, Source: logs.SourceBuild, Job: "1", Msg: line})
		}
		d.Jobs.Details = func(error) (map[string]any, bool) {
			return map[string]any{"stage": "download", "error": "GET https://user:" + urlPass + "@example.com/x.pbf: 403",
				"log_tail": []string{"X-Goog-Api-Key: " + goog, "password=tailsecret99"}}, true
		}
	})
	events, cancelEvents := e.jobs.Hub().Subscribe()
	defer cancelEvents()
	e.jobs.Register("build", func(_ context.Context, j *jobs.Job) error {
		j.Stage("download")
		j.Logf("X-Goog-Api-Key: %s", goog)
		j.Logf("the admin password is %s", adminLiteral)
		j.Progress(0.5, "fetching https://example.com/x.pbf?token=progresstoken77")
		return errors.New("download failed: GET https://user:" + urlPass + "@example.com/x.pbf: 403")
	})
	c := e.login()
	if r, b := e.do(c, "POST", "/admin/api/build", map[string]any{}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 202 {
		t.Fatalf("build: %d %s", r.StatusCode, b)
	}
	waitFor(t, func() bool {
		j, err := e.st.Job(context.Background(), 1)
		return err == nil && j.Status == jobs.StatusFailed
	})
	clean := func(where, text string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s contains the secret %q:\n%s", where, s, text)
			}
		}
		if !strings.Contains(text, logs.Mask) {
			t.Errorf("%s shows no mask at all; did the job's output get there?\n%s", where, text)
		}
	}
	// What was stored.
	ctx := context.Background()
	row, _ := e.st.Job(ctx, 1)
	rowJSON, _ := json.Marshal(row)
	clean("the stored job row", string(rowJSON))
	tail, _ := e.st.JobLogTail(ctx, 1, 0)
	tailJSON, _ := json.Marshal(tail)
	clean("the stored job log", string(tailJSON))
	hist, _ := e.st.History(ctx, "job", 10)
	histJSON, _ := json.Marshal(hist)
	clean("the stored history", string(histJSON))
	// What was published while it ran.
	var published strings.Builder
drain:
	for {
		select {
		case ev := <-events:
			b, _ := json.Marshal(ev.Data)
			published.Write(b)
		default:
			break drain
		}
	}
	clean("the job event stream", published.String())
	// What the admin API answers.
	for _, path := range []string{"/admin/api/jobs", "/admin/api/jobs/1", "/admin/api/history", "/admin/api/overview", "/admin/api/logs"} {
		r, b := e.do(c, "GET", path, nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", path, r.StatusCode)
		}
		clean(path, string(b))
	}
}

// Rows stored raw (an older version, or no redactor on the runner) are masked on the way out.
func TestStoredJobTextIsRedactedAtTheAPI(t *testing.T) {
	const goog = "AIzaSyA-1234567890abcdefghijklmnopqrstuv"
	e := newEnv(t, false) // no Runner.Redact, no sink
	e.jobs.Details = func(error) (map[string]any, bool) {
		return map[string]any{"log_tail": []string{"X-Goog-Api-Key: " + goog}}, true
	}
	e.jobs.Register("build", func(_ context.Context, j *jobs.Job) error {
		j.Logf("X-Goog-Api-Key: %s", goog)
		return errors.New("upstream said key " + goog + " is invalid")
	})
	c := e.login()
	e.do(c, "POST", "/admin/api/build", map[string]any{}, map[string]string{"Origin": e.ts.URL})
	waitFor(t, func() bool {
		j, err := e.st.Job(context.Background(), 1)
		return err == nil && j.Status == jobs.StatusFailed
	})
	if row, _ := e.st.Job(context.Background(), 1); !strings.Contains(row.Error, goog) {
		t.Fatal("test setup: the row should be stored raw here")
	}
	for _, path := range []string{"/admin/api/jobs", "/admin/api/jobs/1", "/admin/api/history", "/admin/api/overview"} {
		_, b := e.do(c, "GET", path, nil, nil)
		if strings.Contains(string(b), goog) || !strings.Contains(string(b), logs.Mask) {
			t.Errorf("%s shows a stored secret:\n%s", path, b)
		}
	}
}

// Review 1, finding 6: a live tail whose client stops reading is dropped and unsubscribed,
// and the number of live tails is bounded.
func TestStalledLogStreamIsDropped(t *testing.T) {
	old := streamWriteTimeout
	streamWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { streamWriteTimeout = old })
	e, sink := newLogEnv(t)
	c := e.login()
	u, _ := url.Parse(e.ts.URL)
	var cookie string
	for _, ck := range c.Jar.Cookies(u) {
		cookie = ck.Name + "=" + ck.Value
	}
	// A real connection that sends the request and then never reads.
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10) // fill up sooner
	}
	_, _ = fmt.Fprintf(conn, "GET /admin/api/logs/stream HTTP/1.1\r\nHost: %s\r\nCookie: %s\r\n\r\n", u.Host, cookie)
	waitFor(t, func() bool { return sink.Subscribers() == 1 })

	// Far more than the socket buffers hold; the writer must never block on it.
	line := strings.Repeat("x", 8<<10)
	wrote := make(chan struct{})
	go func() {
		for i := range 4000 {
			sink.Log("child", logs.LevelInfo, "", fmt.Sprintf("%05d %s", i, line))
			if i%200 == 0 {
				time.Sleep(20 * time.Millisecond) // let the stream's batches go out
			}
		}
		close(wrote)
	}()
	select {
	case <-wrote:
	case <-time.After(20 * time.Second):
		t.Fatal("log writers blocked behind a stalled stream")
	}
	deadline := time.Now().Add(10 * time.Second)
	for sink.Subscribers() != 0 && time.Now().Before(deadline) {
		sink.Log("child", logs.LevelInfo, "", line) // keep something to write
		time.Sleep(50 * time.Millisecond)
	}
	if n := sink.Subscribers(); n != 0 {
		t.Fatalf("the stalled stream is still subscribed (%d subscribers)", n)
	}
	if n := e.srv.logStreams.Load(); n != 0 {
		t.Errorf("stream count is %d after the stream ended", n)
	}
}

func TestLogStreamCap(t *testing.T) {
	old := maxLogStreams
	maxLogStreams = 2
	t.Cleanup(func() { maxLogStreams = old })
	e, sink := newLogEnv(t)
	c := e.login()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := func() *http.Response {
		req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/admin/api/logs/stream", nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	a, b := open(), open()
	defer func() { _ = a.Body.Close(); _ = b.Body.Close() }()
	if a.StatusCode != 200 || b.StatusCode != 200 {
		t.Fatalf("first two streams: %d %d", a.StatusCode, b.StatusCode)
	}
	third := open()
	body, _ := io.ReadAll(third.Body)
	_ = third.Body.Close()
	if third.StatusCode != 503 || !strings.Contains(string(body), "too_many_streams") || third.Header.Get("Retry-After") == "" {
		t.Fatalf("third stream: %d %s", third.StatusCode, body)
	}
	// Closing one frees a place.
	_ = a.Body.Close()
	waitFor(t, func() bool { return sink.Subscribers() == 1 })
	fourth := open()
	defer func() { _ = fourth.Body.Close() }()
	if fourth.StatusCode != 200 {
		t.Fatalf("after one closed: %d", fourth.StatusCode)
	}
}
