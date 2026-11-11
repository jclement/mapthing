package logs

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	const key = "mt_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	const goog = "AIzaSyA-1234567890abcdefghijklmnopqrstuv"
	cases := []struct{ name, in string }{
		{"query key", "GET /styles/overlay.json?key=" + key + " 200"},
		{"query key not ours", "GET /tiles/1/2/3.mvt?key=hunter2hunter2&x=1"},
		{"api_key query", "GET /ors/v2/directions/driving-car?api_key=sekritsekrit&start=1,2"},
		{"bearer header", "Authorization: Bearer " + key},
		{"bare authorization (ORS style)", "authorization=" + key},
		{"authorization json", `{"Authorization":"Basic dXNlcjpodW50ZXIy"}`},
		{"header map", "headers: map[Authorization:[Bearer s3cr3t-s3cr3t-value] Accept:[*/*]]"},
		{"google header", "X-Goog-Api-Key: " + goog},
		{"google header lower", "x-goog-api-key=" + goog},
		{"google key bare", "upstream said key " + goog + " is invalid"},
		{"internal token header", "X-Mapthing-Internal: 0123456789abcdef0123456789abcdef"},
		{"env password", "ADMIN_PASSWORD=correct-horse-battery"},
		{"env token", "TUNNEL_TOKEN=eyJhIjoiYWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoifQ"},
		{"json password", `{"username":"admin","password":"correct horse battery"}`},
		{"slog token", `msg="renderer start" internal_token=deadbeefdeadbeefdeadbeef`},
		{"url userinfo", "fetching https://user:hunter2hunter2@example.com/tiles"},
		{"bare key", "key " + key + " was revoked"},
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop"},
		{"cookie", "Cookie: mapthing_session=0123456789abcdef0123456789abcdef"},
		{"escaped json", `body="{\"password\":\"hunter2hunter2\"}"`},
	}
	secrets := []string{key, key[3:], goog, "hunter2hunter2", "sekritsekrit", "dXNlcjpodW50ZXIy", "s3cr3t-s3cr3t-value",
		"0123456789abcdef0123456789abcdef", "correct-horse-battery", "correct horse battery", "eyJhIjoi",
		"deadbeefdeadbeefdeadbeef", "eyJhbGciOiJIUzI1NiJ9", "abcdefghijklmnop"}
	for _, c := range cases {
		got := Redact(c.in)
		if !strings.Contains(got, Mask) {
			t.Errorf("%s: nothing redacted in %q", c.name, got)
		}
		for _, s := range secrets {
			if strings.Contains(c.in, s) && strings.Contains(got, s) {
				t.Errorf("%s: %q survived: %q", c.name, s, got)
			}
		}
	}
}

func TestRedactKeepsOrdinaryLines(t *testing.T) {
	for _, in := range []string{
		"child state child=valhalla@8002 state=running msg=\"\"",
		"http method=GET path=/tiles/basemap/10/200/300.mvt status=200 bytes=18233 dur=3ms",
		"2026/11/11 10:00:00.123456 [INFO] Tile extract successfully loaded with 4821 tiles",
		"0:01:12 INF [archive] - z14 tiles: 120,000 (avg 12kB)",
		"stage terrain done in 812.4s",
		"generation 20261109T130030Z is live (previous 20261102T030012Z)",
		"job start job=12 name=build trigger=schedule",
	} {
		if got := Redact(in); got != in {
			t.Errorf("changed an ordinary line:\n  in  %q\n  out %q", in, got)
		}
	}
}

func TestRedactorLiterals(t *testing.T) {
	var r Redactor
	r.AddSecret("tr0ub4dor&3")
	r.AddSecret("abc") // too short: ignored
	got := r.Redact("login failed for admin with tr0ub4dor&3 (abc)")
	if strings.Contains(got, "tr0ub4dor") || !strings.Contains(got, "(abc)") {
		t.Fatalf("got %q", got)
	}
}

func TestSinkCapturesAndRedacts(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer
	s := New(Options{Dir: dir, Console: &console})
	defer s.Close()
	s.AddSecret("open-sesame-123")

	slog.New(s.Handler(slog.LevelInfo)).Warn("login failed", "ip", "10.0.0.9", "password", "open-sesame-123")
	s.Log("valhalla@8002", "", "", "2026/11/11 10:00:00 [ERROR] could not open tiles")
	s.Log(SourceBuild, "", "7", "$ curl https://example.com/x?key=AIzaSyA-1234567890abcdefghijklmnopqrstuv")
	slog.New(s.Handler(slog.LevelInfo)).Debug("dropped: below the level")

	got, more := s.Query(Filter{}, 10)
	if more || len(got) != 3 {
		t.Fatalf("got %d entries (more=%v): %+v", len(got), more, got)
	}
	if got[0].Source != SourceServer || got[0].Level != LevelWarn || !strings.Contains(got[0].Msg, "ip=10.0.0.9") {
		t.Errorf("server entry: %+v", got[0])
	}
	if got[1].Source != "valhalla@8002" || got[1].Level != LevelError {
		t.Errorf("child entry: %+v", got[1])
	}
	if got[2].Job != "7" || got[2].Source != SourceBuild {
		t.Errorf("build entry: %+v", got[2])
	}
	if got[0].ID >= got[1].ID || got[1].ID >= got[2].ID {
		t.Errorf("ids not increasing: %d %d %d", got[0].ID, got[1].ID, got[2].ID)
	}
	file, err := os.ReadFile(filepath.Join(dir, currentName))
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"file": string(file), "console": console.String(), "memory": fmt.Sprint(got)} {
		if strings.Contains(text, "open-sesame") || strings.Contains(text, "AIzaSy") {
			t.Errorf("a secret reached the %s: %s", name, text)
		}
		if !strings.Contains(text, "could not open tiles") {
			t.Errorf("%s lacks the child line", name)
		}
	}
	if src := s.Sources(); len(src) != 3 || src[0] != SourceServer {
		t.Errorf("sources %v", src)
	}
}

func TestSinkFilters(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	s := New(Options{Now: func() time.Time { now = now.Add(time.Minute); return now }})
	s.Log("a", LevelInfo, "", "alpha one")
	s.Log("b", LevelWarn, "", "Bravo two")
	s.Log("a", LevelError, "3", "charlie three")
	s.Log("b", LevelDebug, "", "delta four")

	check := func(name string, f Filter, want ...string) {
		t.Helper()
		got, _ := s.Query(f, 0)
		var msgs []string
		for _, e := range got {
			msgs = append(msgs, strings.Fields(e.Msg)[0])
		}
		if strings.Join(msgs, ",") != strings.Join(want, ",") {
			t.Errorf("%s: got %v, want %v", name, msgs, want)
		}
	}
	check("all", Filter{}, "alpha", "Bravo", "charlie", "delta")
	check("source", Filter{Sources: []string{"a"}}, "alpha", "charlie")
	check("level", Filter{MinLevel: LevelWarn}, "Bravo", "charlie")
	check("text", Filter{Text: "BRAVO"}, "Bravo")
	check("job", Filter{Job: "3"}, "charlie")
	check("since", Filter{Since: time.Date(2026, 11, 11, 12, 3, 0, 0, time.UTC)}, "charlie", "delta")
	check("until", Filter{Until: time.Date(2026, 11, 11, 12, 3, 0, 0, time.UTC)}, "alpha", "Bravo")
	all, _ := s.Query(Filter{}, 0)
	check("before", Filter{Before: all[2].ID}, "alpha", "Bravo")
	check("after", Filter{After: all[1].ID}, "charlie", "delta")

	page, more := s.Query(Filter{}, 2)
	if !more || len(page) != 2 || page[0].Msg != "charlie three" {
		t.Errorf("newest page: %+v more=%v", page, more)
	}
}

func TestSinkRotationBoundsAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	const maxBytes = 64 << 10
	s := New(Options{Dir: dir, MaxBytes: maxBytes, Ring: 50})
	line := strings.Repeat("x", 200)
	const n = 2000 // ~500 KB of entries through a 64 KB store
	for i := range n {
		s.Log("child", LevelInfo, "", fmt.Sprintf("line %05d %s", i, line))
	}
	var total int64
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		fi, _ := e.Info()
		total += fi.Size()
	}
	if total > maxBytes {
		t.Errorf("files hold %d bytes, bound is %d", total, maxBytes)
	}
	if len(ents) < 3 {
		t.Errorf("expected several segments, got %d files", len(ents))
	}
	// Older than the ring (50 entries): the query must read the files.
	got, more := s.Query(Filter{}, 120)
	if len(got) != 120 || !more {
		t.Fatalf("got %d entries, more=%v", len(got), more)
	}
	if !strings.HasPrefix(got[119].Msg, fmt.Sprintf("line %05d", n-1)) || !strings.HasPrefix(got[0].Msg, fmt.Sprintf("line %05d", n-120)) {
		t.Errorf("wrong window: %q … %q", got[0].Msg[:10], got[119].Msg[:10])
	}
	// Paging back from the oldest of that page continues without a gap.
	older, _ := s.Query(Filter{Before: got[0].ID}, 10)
	if len(older) != 10 || !strings.HasPrefix(older[9].Msg, fmt.Sprintf("line %05d", n-121)) {
		t.Errorf("paging back: %d entries, last %q", len(older), older[len(older)-1].Msg[:10])
	}
	last := got[119].ID
	st := s.Status()
	if st.Oldest == nil || st.Bytes == 0 || st.Bytes > maxBytes {
		t.Errorf("status %+v", st)
	}
	s.Close()

	// A new process: the history is still there, and new ids sort after the old ones.
	s2 := New(Options{Dir: dir, MaxBytes: maxBytes, Ring: 50, Now: func() time.Time { return time.Unix(0, 0) }})
	defer s2.Close()
	s2.Log("child", LevelInfo, "", "after restart")
	got2, _ := s2.Query(Filter{}, 3)
	if len(got2) != 3 || got2[2].Msg != "after restart" || !strings.HasPrefix(got2[1].Msg, fmt.Sprintf("line %05d", n-1)) {
		t.Fatalf("after restart: %+v", got2)
	}
	if got2[2].ID <= last {
		t.Errorf("id went backwards across a restart: %d <= %d", got2[2].ID, last)
	}
	count := 0
	s2.Each(Filter{Text: "line"}, func(Entry) bool { count++; return true })
	if count < 100 || count >= n {
		t.Errorf("Each saw %d entries (the files hold a bounded window)", count)
	}
	if src := s2.Sources(); len(src) != 1 || src[0] != "child" {
		t.Errorf("sources after restart: %v", src)
	}
}

func TestSubscribe(t *testing.T) {
	s := New(Options{})
	ch, dropped, cancel := s.Subscribe()
	s.Log("a", LevelInfo, "", "hello ?key=supersecretvalue")
	select {
	case e := <-ch:
		if strings.Contains(e.Msg, "supersecret") {
			t.Errorf("subscriber saw a secret: %q", e.Msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no entry")
	}
	for range 2000 {
		s.Log("a", LevelInfo, "", "flood")
	}
	if dropped() == 0 {
		t.Error("a full subscriber should lose lines, not block the writer")
	}
	cancel()
}

func TestDetectLevel(t *testing.T) {
	for line, want := range map[string]string{
		"2026/11/11 10:00:00.1 [ERROR] bad":         LevelError,
		"2026/11/11 10:00:00.1 [WARN] slow":         LevelWarn,
		"0:00:03 WAR [osm] - something odd":         LevelWarn,
		"0:00:03 ERR [osm] - something broke":       LevelError,
		"0:00:03 INF [osm] - fine":                  LevelInfo,
		"Error: Cannot find module 'x'":             LevelError,
		"time=… level=WARN msg=x":                   LevelWarn,
		"listening on 127.0.0.1:8081":               LevelInfo,
		"2026/11/11 10:00:00.1 [DEBUG] chatter":     LevelDebug,
		"Tile extract successfully loaded":          LevelInfo,
		"stage terrain done; 0 errors in 812 tiles": LevelInfo,
	} {
		if got := DetectLevel(line); got != want {
			t.Errorf("%q: got %s, want %s", line, got, want)
		}
	}
}

func TestChildLine(t *testing.T) {
	cases := []struct{ in, level, msg string }{
		{`{"time":"2026-11-11T16:34:08.896Z","level":"INFO","msg":"renderer listening","addr":"127.0.0.1:8081","pool":2}`, LevelInfo, "renderer listening addr=127.0.0.1:8081 pool=2"},
		{`{"level":"ERROR","msg":"render failed","err":"tile 404 at z14"}`, LevelError, `render failed err="tile 404 at z14"`},
		{`{"not":"a log line"}`, "", `{"not":"a log line"}`},
		{"3 2026/11/11 16:34:19.185378 GET /status HTTP/1.1", LevelDebug, ""},
		{"3 2026/11/11 16:34:19.186026 200 363", LevelDebug, ""},
		{"7 2026/11/11 16:34:19.186026 503 41", LevelWarn, ""},
		{"2026-11-11 16:34:19.185469324 [INFO] Got Loki Request 3", LevelDebug, ""},
		{"2026-11-11 16:34:08.744729230 [INFO] algorithm::time_dependent_forward_a*", LevelDebug, ""},
		{"2026-11-11 16:35:26.679654335 \x1b[32;1m[INFO]\x1b[0m Got Loki Request 2", LevelDebug, "2026-11-11 16:35:26.679654335 [INFO] Got Loki Request 2"},
		{"2026-11-11 16:34:08.314635884 [WARN] Traffic tile extract could not be loaded", "", ""},
		{"2026-11-11 16:34:07.1 [INFO] Tile extract successfully loaded with 4821 tiles", "", ""},
	}
	for _, c := range cases {
		level, msg := ChildLine(c.in)
		want := c.msg
		if want == "" {
			want = c.in
		}
		if level != c.level || msg != want {
			t.Errorf("%q:\n  got  %q %q\n  want %q %q", c.in, level, msg, c.level, want)
		}
	}
}

func TestMinLevelDropsDebug(t *testing.T) {
	s := New(Options{MinLevel: LevelInfo})
	s.Log("valhalla@8002", LevelDebug, "", "chatter")
	s.Log("valhalla@8002", LevelInfo, "", "kept")
	if got, _ := s.Query(Filter{}, 0); len(got) != 1 || got[0].Msg != "kept" {
		t.Fatalf("got %+v", got)
	}
}
