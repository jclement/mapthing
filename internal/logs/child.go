package logs

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ChildLine turns one line of a child's output into a level and a message.
//
//   - A JSON log line ({"level":…,"msg":…}, which the renderer writes) becomes its message
//     followed by the other fields as key=value, with its own level.
//   - Valhalla's per-request chatter (prime_server's request and response lines and the
//     "Got Loki Request" lines, five of them for every health probe) is debug, so it is only
//     kept when the server runs with MAPTHING_DEBUG. A 5xx response line is a warning.
//   - Anything else keeps its text; level "" lets the sink work it out (DetectLevel).
func ChildLine(line string) (level, msg string) {
	line = StripANSI(line)
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
		if lv, m, ok := jsonLine(t); ok {
			return lv, m
		}
	}
	if m := primeResponse.FindStringSubmatch(t); m != nil {
		if m[1] >= "500" {
			return LevelWarn, line
		}
		return LevelDebug, line
	}
	if primeRequest.MatchString(t) || valhallaWorker.MatchString(t) {
		return LevelDebug, line
	}
	return "", line
}

// StripANSI removes terminal colour and cursor sequences (Valhalla colours its level tags).
func StripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	return ansi.ReplaceAllString(s, "")
}

var (
	ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	// "12 2026/11/11 16:34:08.682895 GET /status HTTP/1.1"
	primeRequest = regexp.MustCompile(`^\d+ \d{4}/\d\d/\d\d \d\d:\d\d:\d\d\.\d+ [A-Z]+ \S+ HTTP/\d`)
	// "12 2026/11/11 16:34:08.683594 200 363"
	primeResponse = regexp.MustCompile(`^\d+ \d{4}/\d\d/\d\d \d\d:\d\d:\d\d\.\d+ (\d{3}) \d+$`)
	// "2026-11-11 16:34:08.683017879 [INFO] Got Loki Request 0"
	valhallaWorker = regexp.MustCompile(`\[INFO\] (?:Got (?:Loki|Thor|Odin) Request \d+|algorithm::\S+)$`)
)

func jsonLine(t string) (level, msg string, ok bool) {
	var m map[string]any
	if json.Unmarshal([]byte(t), &m) != nil {
		return "", "", false
	}
	text, isStr := m["msg"].(string)
	if !isStr {
		if text, isStr = m["message"].(string); !isStr {
			return "", "", false
		}
	}
	lv, _ := m["level"].(string)
	switch strings.ToLower(lv) {
	case "debug", "trace":
		level = LevelDebug
	case "warn", "warning":
		level = LevelWarn
	case "error", "fatal", "panic", "critical":
		level = LevelError
	default:
		level = LevelInfo
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		switch k {
		case "msg", "message", "level", "time", "ts", "timestamp":
		default:
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(text)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		var s string
		switch v := m[k].(type) {
		case string:
			s = v
		default:
			j, _ := json.Marshal(v)
			s = string(j)
		}
		if s == "" || strings.ContainsAny(s, " \t\n\"=") {
			s = strconv.Quote(s)
		}
		b.WriteString(s)
	}
	return level, b.String(), true
}
