package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/logs"
)

// logFilter reads the filter shared by the three logs endpoints from the query:
// source=a,b (or repeated), level=debug|info|warn|error (that level and above), q=text,
// job=<id>, since=/until= (RFC 3339), before=/after= (entry ids).
func logFilter(r *http.Request) (logs.Filter, error) {
	q := r.URL.Query()
	var f logs.Filter
	for _, v := range q["source"] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				f.Sources = append(f.Sources, s)
			}
		}
	}
	f.MinLevel = strings.ToLower(q.Get("level"))
	if f.MinLevel == "all" {
		f.MinLevel = ""
	}
	if !logs.ValidLevel(f.MinLevel) {
		return f, fmt.Errorf("level must be debug, info, warn or error")
	}
	f.Text = q.Get("q")
	f.Job = q.Get("job")
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, fmt.Errorf("%s must be an RFC 3339 time", name)
			}
			*dst = t
		}
	}
	for name, dst := range map[string]*int64{"before": &f.Before, "after": &f.After} {
		if v := q.Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return f, fmt.Errorf("%s must be an entry id", name)
			}
			*dst = n
		}
	}
	return f, nil
}

func (s *Server) logSink(w http.ResponseWriter, r *http.Request) (*logs.Sink, logs.Filter, bool) {
	if s.d.Logs == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "this server keeps no log")
		return nil, logs.Filter{}, false
	}
	f, err := logFilter(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return nil, f, false
	}
	return s.d.Logs, f, true
}

// logsQuery: GET /admin/api/logs?… → {entries (oldest first), more, sources, store}. The newest
// `limit` (default 500, max 5000) matching entries; page back with before=<oldest id shown>.
func (s *Server) logsQuery(w http.ResponseWriter, r *http.Request) {
	sink, f, ok := s.logSink(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	limit = min(limit, 5000)
	entries, more := sink.Query(f, limit)
	if entries == nil {
		entries = []logs.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "more": more, "sources": sink.Sources(), "store": sink.Status()})
}

// logsDownload: GET /admin/api/logs/download?…&format=text|jsonl → every matching entry as a
// file, oldest first.
func (s *Server) logsDownload(w http.ResponseWriter, r *http.Request) {
	sink, f, ok := s.logSink(w, r)
	if !ok {
		return
	}
	jsonl := r.URL.Query().Get("format") == "jsonl"
	name := "mapthing-logs-" + time.Now().UTC().Format("20060102T150405Z")
	if jsonl {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		name += ".jsonl"
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		name += ".log"
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	sink.Each(f, func(e logs.Entry) bool {
		var err error
		if jsonl {
			err = enc.Encode(e)
		} else {
			_, err = fmt.Fprintln(w, logs.FormatText(e))
		}
		return err == nil && r.Context().Err() == nil
	})
}

// logsStream: GET /admin/api/logs/stream?… (SSE) → event "entry" per matching entry as it is
// written. With after=<id> the entries since then come first, so a page that loaded history
// and then connects misses nothing. Event "dropped" {count} when the reader fell behind.
func (s *Server) logsStream(w http.ResponseWriter, r *http.Request) {
	sink, f, ok := s.logSink(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	ch, dropped, cancel := sink.Subscribe()
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	send := func(e logs.Entry) {
		b, _ := json.Marshal(e)
		_, _ = fmt.Fprintf(w, "event: entry\ndata: %s\n\n", b)
	}
	var last int64
	if f.After != 0 {
		// Subscribed first, so nothing falls between the catch-up and the live stream;
		// duplicates are skipped by id below.
		catchUp, _ := sink.Query(f, 5000)
		for _, e := range catchUp {
			send(e)
			last = e.ID
		}
	}
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	flush := time.NewTicker(200 * time.Millisecond) // batch bursts (a build prints hundreds of lines a second)
	defer flush.Stop()
	dirty := false
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if e.ID > last && f.Match(e) {
				send(e)
				dirty = true
			}
		case <-flush.C:
			if n := dropped(); n > 0 {
				_, _ = fmt.Fprintf(w, "event: dropped\ndata: {\"count\":%d}\n\n", n)
				dirty = true
			}
			if dirty {
				fl.Flush()
				dirty = false
			}
		case <-tick.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}
