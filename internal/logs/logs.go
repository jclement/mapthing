// Package logs is mapthing's one log: the server's own slog output, every supervised child's
// output and build-job output, each line tagged with its source, level and time (and job id
// for builds). Lines are redacted, kept in a ring buffer in memory, written to size-bounded
// rotating files so they survive a restart, echoed to the console (docker logs) and fanned out
// to live subscribers (the admin Logs page).
package logs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Levels, least to most severe.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// SourceServer is the source of the server's own log lines.
const SourceServer = "mapthing"

// SourceBuild is the source of build-job output.
const SourceBuild = "build"

// Entry is one log line.
type Entry struct {
	// ID orders entries and is unique across restarts: the time in nanoseconds, nudged forward
	// when two lines share one. It is a string in JSON (it does not fit a JavaScript number).
	ID     int64     `json:"id,string"`
	Time   time.Time `json:"t"`
	Source string    `json:"source"`
	Level  string    `json:"level"`
	Job    string    `json:"job,omitempty"`
	Msg    string    `json:"msg"`
}

// Options configure a Sink.
type Options struct {
	// Dir holds the log files ("" = memory only).
	Dir string
	// MaxBytes bounds the files' total size (default 50 MB; ≤ 0 with Dir set means the default).
	MaxBytes int64
	// Ring is how many entries stay in memory (default 5000).
	Ring int
	// MinLevel drops entries below it ("" keeps everything). The server passes info unless
	// MAPTHING_DEBUG is set, so debug chatter is neither stored nor printed.
	MinLevel string
	// Console receives every line as text (nil = none). The server passes os.Stderr.
	Console io.Writer
	// Now is the clock (tests).
	Now func() time.Time
}

const (
	currentName = "log-current.jsonl"
	segPrefix   = "log-"
	segSuffix   = ".jsonl"
	// maxLine caps one message; longer ones are cut (a tool dumping a megabyte on one line).
	maxLine = 16 << 10
)

// Sink collects log entries. The zero value is not usable; call New.
type Sink struct {
	Redactor

	opt     Options
	segSize int64

	// fileMu guards the set of files: rotation holds it for writing, a scan for reading. It is
	// taken after mu, never before.
	fileMu sync.RWMutex

	mu      sync.Mutex
	ring    []Entry // circular, next is the write position once full
	next    int
	full    bool
	lastID  int64
	file    *os.File
	size    int64
	sources map[string]struct{}
	subs    map[chan Entry]struct{}
	dropped map[chan Entry]*int
	fileErr string
}

// New opens a sink. A directory that cannot be used is not fatal: the sink then keeps logs in
// memory only and says so on the console.
func New(opt Options) *Sink {
	if opt.Ring <= 0 {
		opt.Ring = 5000
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = 50 << 20
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	s := &Sink{opt: opt, ring: make([]Entry, 0, opt.Ring), sources: map[string]struct{}{},
		subs: map[chan Entry]struct{}{}, dropped: map[chan Entry]*int{}}
	// Ten segments: rotation then drops a tenth of the history at a time.
	s.segSize = max(opt.MaxBytes/10, 4<<10)
	if opt.Dir != "" {
		if err := s.openFile(); err != nil {
			s.fileErr = err.Error()
			if opt.Console != nil {
				_, _ = fmt.Fprintf(opt.Console, "logs: not writing log files: %v\n", err)
			}
		} else {
			s.lastID = s.lastIDOnDisk()
			for _, src := range s.sourcesOnDisk() {
				s.sources[src] = struct{}{}
			}
		}
	}
	return s
}

func (s *Sink) openFile() error {
	if err := os.MkdirAll(s.opt.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.opt.Dir, currentName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	s.file, s.size = f, fi.Size()
	return nil
}

// Close closes the log file.
func (s *Sink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// Log records one line. An empty level is worked out from the text (DetectLevel). Multi-line
// messages are kept as one entry.
func (s *Sink) Log(source, level, job, msg string) {
	s.Add(Entry{Source: source, Level: level, Job: job, Msg: msg})
}

// Add records an entry. Time, ID and Level are filled in when zero.
func (s *Sink) Add(e Entry) {
	if s == nil {
		return
	}
	e.Msg = strings.TrimRight(StripANSI(e.Msg), "\r\n")
	if len(e.Msg) > maxLine {
		e.Msg = e.Msg[:maxLine] + " … [cut]"
	}
	e.Msg = s.Redact(strings.ToValidUTF8(e.Msg, "?"))
	if e.Level == "" {
		e.Level = DetectLevel(e.Msg)
	}
	if s.opt.MinLevel != "" && levelRank(e.Level) < levelRank(s.opt.MinLevel) {
		return
	}
	if e.Source == "" {
		e.Source = SourceServer
	}
	if e.Time.IsZero() {
		e.Time = s.opt.Now()
	}
	e.Time = e.Time.UTC()

	s.mu.Lock()
	e.ID = max(e.Time.UnixNano(), s.lastID+1)
	s.lastID = e.ID
	s.sources[e.Source] = struct{}{}
	if len(s.ring) < cap(s.ring) {
		s.ring = append(s.ring, e)
	} else {
		s.ring[s.next] = e
		s.next = (s.next + 1) % len(s.ring)
		s.full = true
	}
	if s.file != nil {
		if b, err := json.Marshal(e); err == nil {
			b = append(b, '\n')
			if n, err := s.file.Write(b); err == nil {
				s.size += int64(n)
				if s.size >= s.segSize {
					s.rotateLocked()
				}
			}
		}
	}
	for c := range s.subs {
		select {
		case c <- e:
		default:
			*s.dropped[c]++
		}
	}
	console := s.opt.Console
	s.mu.Unlock()

	if console != nil {
		_, _ = io.WriteString(console, FormatText(e)+"\n")
	}
}

// FormatText renders an entry as one console or download line.
func FormatText(e Entry) string {
	var b strings.Builder
	b.WriteString(e.Time.UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(e.Level))
	for i := len(e.Level); i < 5; i++ {
		b.WriteByte(' ')
	}
	b.WriteString(" [")
	b.WriteString(e.Source)
	if e.Job != "" {
		b.WriteString(" #")
		b.WriteString(e.Job)
	}
	b.WriteString("] ")
	b.WriteString(e.Msg)
	return b.String()
}

// rotateLocked closes the current file under a dated name and deletes the oldest segments
// until the total fits MaxBytes.
func (s *Sink) rotateLocked() {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	_ = s.file.Close()
	s.file = nil
	cur := filepath.Join(s.opt.Dir, currentName)
	// The name is the id of the last entry in the segment: unique, and it sorts by time.
	seg := filepath.Join(s.opt.Dir, fmt.Sprintf("%s%020d%s", segPrefix, s.lastID, segSuffix))
	_ = os.Rename(cur, seg)
	if err := s.openFile(); err != nil {
		s.fileErr = err.Error()
		return
	}
	segs := s.segments()
	var total int64
	sizes := make([]int64, len(segs))
	for i, p := range segs {
		if fi, err := os.Stat(p); err == nil {
			sizes[i] = fi.Size()
			total += fi.Size()
		}
	}
	// Leave room for the file being written, so the total stays under the bound.
	for i := 0; i < len(segs) && total+s.segSize > s.opt.MaxBytes; i++ {
		if os.Remove(segs[i]) == nil {
			total -= sizes[i]
		}
	}
}

// segments lists the rotated files, oldest first (not the current file).
func (s *Sink) segments() []string {
	ents, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if n != currentName && strings.HasPrefix(n, segPrefix) && strings.HasSuffix(n, segSuffix) && e.Type().IsRegular() {
			out = append(out, filepath.Join(s.opt.Dir, n))
		}
	}
	sort.Strings(out)
	return out
}

// files lists every log file, oldest first.
func (s *Sink) files() []string {
	if s.opt.Dir == "" {
		return nil
	}
	return append(s.segments(), filepath.Join(s.opt.Dir, currentName))
}

func readFile(path string, fn func(Entry)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4*maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) == nil && e.ID != 0 {
			fn(e)
		}
	}
}

func (s *Sink) lastIDOnDisk() int64 {
	var last int64
	files := s.files()
	for i := len(files) - 1; i >= 0 && last == 0; i-- {
		readFile(files[i], func(e Entry) { last = max(last, e.ID) })
	}
	return last
}

func (s *Sink) sourcesOnDisk() []string {
	seen := map[string]struct{}{}
	for _, f := range s.files() {
		readFile(f, func(e Entry) { seen[e.Source] = struct{}{} })
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// Sources lists every source seen (in the files and since start), sorted, the server first.
func (s *Sink) Sources() []string {
	s.mu.Lock()
	out := make([]string, 0, len(s.sources))
	for k := range s.sources {
		out = append(out, k)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == SourceServer) != (out[j] == SourceServer) {
			return out[i] == SourceServer
		}
		return out[i] < out[j]
	})
	return out
}

// Status describes the store for the UI.
type Status struct {
	Dir       string     `json:"dir,omitempty"`
	Bytes     int64      `json:"bytes"`
	MaxBytes  int64      `json:"max_bytes"`
	Oldest    *time.Time `json:"oldest,omitempty"` // the earliest line kept
	FileError string     `json:"file_error,omitempty"`
}

// Status reports how much is kept and how far back it goes.
func (s *Sink) Status() Status {
	st := Status{Dir: s.opt.Dir, MaxBytes: s.opt.MaxBytes}
	s.mu.Lock()
	st.FileError = s.fileErr
	files := s.files()
	s.mu.Unlock()
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			st.Bytes += fi.Size()
		}
	}
	for _, f := range files {
		var first *time.Time
		readFirst(f, func(e Entry) { t := e.Time; first = &t })
		if first != nil {
			st.Oldest = first
			break
		}
	}
	if st.Oldest == nil {
		if r := s.snapshot(); len(r) > 0 {
			t := r[0].Time
			st.Oldest = &t
		}
	}
	return st
}

func readFirst(path string, fn func(Entry)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4*maxLine)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != 0 {
			fn(e)
			return
		}
	}
}

// snapshot returns the ring's entries, oldest first.
func (s *Sink) snapshot() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.ring))
	if s.full {
		out = append(out, s.ring[s.next:]...)
		out = append(out, s.ring[:s.next]...)
	} else {
		out = append(out, s.ring...)
	}
	return out
}

// Subscribe returns live entries and a cancel func. A subscriber that falls behind loses
// lines rather than blocking the writer; Dropped says how many.
func (s *Sink) Subscribe() (ch <-chan Entry, dropped func() int, cancel func()) {
	c := make(chan Entry, 1024)
	n := new(int)
	s.mu.Lock()
	s.subs[c] = struct{}{}
	s.dropped[c] = n
	s.mu.Unlock()
	return c, func() int {
			s.mu.Lock()
			defer s.mu.Unlock()
			v := *n
			*n = 0
			return v
		}, func() {
			s.mu.Lock()
			delete(s.subs, c)
			delete(s.dropped, c)
			s.mu.Unlock()
		}
}

// DetectLevel guesses a line's level from the conventions the children use (Valhalla's
// "[ERROR]", Planetiler's " ERR ", slog's level=WARN, Node's "Error:"). Unknown is info.
func DetectLevel(line string) string {
	head := line
	if len(head) > 200 {
		head = head[:200]
	}
	up := strings.ToUpper(head)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(up, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("[ERROR]", "[FATAL]", "[CRITICAL]", "LEVEL=ERROR", " ERR ", " ERROR ", "ERROR:", "FATAL:", "PANIC:", "EXCEPTION", "TRACEBACK", "SEGMENTATION FAULT") ||
		strings.HasPrefix(up, "ERROR") || strings.HasPrefix(up, "FATAL") || strings.HasPrefix(up, "PANIC"):
		return LevelError
	case has("[WARN]", "[WARNING]", "LEVEL=WARN", " WAR ", " WARN ", "WARN:", "WARNING:", "DEPRECAT") ||
		strings.HasPrefix(up, "WARN"):
		return LevelWarn
	case has("[DEBUG]", "[TRACE]", "LEVEL=DEBUG", " DEB ", " DEBUG ") || strings.HasPrefix(up, "DEBUG"):
		return LevelDebug
	}
	return LevelInfo
}

func levelRank(l string) int {
	switch l {
	case LevelDebug:
		return 0
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	}
	return 1
}
