package jobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is one server-sent event. Type is the SSE event name ("job", "log", or anything a
// publisher chooses such as "generation"); Data is JSON-encoded.
type Event struct {
	Type string
	Data any
}

// Hub fans events out to subscribers. Slow subscribers drop events rather than block.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Publish sends an event to every subscriber (exported for other packages, e.g. generation swaps).
func (h *Hub) Publish(e Event) { h.publish(e) }

func (h *Hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- e:
		default:
		}
	}
}

// Subscribe returns a channel of events and a cancel func.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	c := make(chan Event, 256)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return c, func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}
}

// Subscribers returns the current subscriber count.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Handler streams events as text/event-stream until the client goes away. A comment line
// is sent every 15 s to keep proxies from closing the stream. On connect, snapshot (if set)
// is called and its events are sent first.
func (h *Hub) Handler(snapshot func() []Event) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ch, cancel := h.Subscribe()
		defer cancel()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "retry: 3000\n\n")
		if snapshot != nil {
			for _, e := range snapshot() {
				writeEvent(w, e)
			}
		}
		fl.Flush()
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-ch:
				writeEvent(w, e)
				fl.Flush()
			case <-tick.C:
				_, _ = fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
			}
		}
	})
}

func writeEvent(w http.ResponseWriter, e Event) {
	b, err := json.Marshal(e.Data)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
}
