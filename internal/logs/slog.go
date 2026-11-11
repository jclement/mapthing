package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Handler returns a slog.Handler that writes records into the sink as source "mapthing":
// the message followed by its attributes as key=value. The sink does the console output, so
// this replaces the process's text handler rather than sitting beside it.
func (s *Sink) Handler(level slog.Leveler) slog.Handler {
	return &handler{sink: s, level: level}
}

type handler struct {
	sink   *Sink
	level  slog.Leveler
	prefix string // group prefix for keys
	attrs  string // preformatted " k=v" pairs from WithAttrs
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	job := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "job" && h.prefix == "" {
			job = a.Value.Resolve().String()
		}
		appendAttr(&b, h.prefix, a)
		return true
	})
	e := Entry{Time: r.Time, Source: SourceServer, Level: slogLevel(r.Level), Job: job, Msg: b.String()}
	h.sink.Add(e)
	return nil
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		appendAttr(&b, h.prefix, a)
	}
	n := *h
	n.attrs = b.String()
	return &n
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.prefix = h.prefix + name + "."
	return &n
}

func slogLevel(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return LevelError
	case l >= slog.LevelWarn:
		return LevelWarn
	case l >= slog.LevelInfo:
		return LevelInfo
	}
	return LevelDebug
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, ga := range v.Group() {
			appendAttr(b, p, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	var s string
	switch v.Kind() {
	case slog.KindString:
		s = v.String()
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			s = err.Error()
		} else {
			s = fmt.Sprint(v.Any())
		}
	default:
		s = v.String()
	}
	if s == "" || strings.ContainsAny(s, " \t\n\"=") {
		s = strconv.Quote(s)
	}
	b.WriteString(s)
}
