package stats

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Traffic keeps, in memory, how many requests of each kind were served in each of the last
// 24 hours' minutes and how long they took, for the admin overview: request rate and p50/p95
// latency over the last hour and day. It starts empty at every start of the server.
type Traffic struct {
	now func() time.Time

	mu    sync.Mutex
	kinds map[string]*trafficKind
}

// TrafficKinds are the kinds the overview shows, in order.
var TrafficKinds = []string{"tiles", "raster", "route", "elevation", "style", "pack"}

const trafficMinutes = 24 * 60

// Latency histogram: bucket i holds durations up to bucketBound(i); the bounds grow by a
// factor of 2^(1/4) from 0.25 ms, so a quantile is within 19% of the true value and 96
// buckets reach past 4 hours.
const histBuckets = 96

func bucketBound(i int) float64 { return 0.25 * math.Pow(2, float64(i)/4) } // milliseconds

func bucketOf(ms float64) int {
	if ms <= 0.25 {
		return 0
	}
	i := int(math.Ceil(4 * math.Log2(ms/0.25)))
	return min(max(i, 0), histBuckets-1)
}

type trafficMinute struct {
	minute   int64 // unix minute this slot holds
	count    uint32
	errors   uint32 // 5xx
	rejected uint32 // 401, 403, 429
	hist     [histBuckets]uint32
}

type trafficKind struct {
	slots [trafficMinutes]trafficMinute
}

// NewTraffic returns an empty recorder.
func NewTraffic() *Traffic {
	return &Traffic{now: time.Now, kinds: map[string]*trafficKind{}}
}

// TrafficKind maps a usage kind and request path to the overview's kind ("" = not shown).
// Styles, glyphs and sprites are counted as "tile" usage but shown apart from vector tiles.
func TrafficKind(usageKind, path string) string {
	switch usageKind {
	case KindTile:
		if len(path) >= 7 && path[:7] == "/tiles/" {
			return "tiles"
		}
		return "style"
	case KindRaster:
		return "raster"
	case KindPack:
		return "pack"
	case KindElevation:
		return "elevation"
	case "raster-internal", "":
		return ""
	default: // route, matrix, isochrone, snap, valhalla, compat
		return "route"
	}
}

// Observe records one finished request.
func (t *Traffic) Observe(kind string, d time.Duration, status int) {
	if t == nil || kind == "" {
		return
	}
	minute := t.now().Unix() / 60
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.kinds[kind]
	if k == nil {
		k = &trafficKind{}
		t.kinds[kind] = k
	}
	s := &k.slots[minute%trafficMinutes]
	if s.minute != minute {
		*s = trafficMinute{minute: minute}
	}
	s.count++
	switch {
	case status >= 500:
		s.errors++
	case status == 401 || status == 403 || status == 429:
		s.rejected++
	}
	s.hist[bucketOf(float64(d)/float64(time.Millisecond))]++
}

// Window summarises one kind over a span of time.
type Window struct {
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`   // answered 5xx
	Rejected int64   `json:"rejected"` // answered 401, 403 or 429
	PerMin   float64 `json:"per_min"`  // mean over the window (or since start, when shorter)
	P50ms    float64 `json:"p50_ms"`
	P95ms    float64 `json:"p95_ms"`
}

// KindTraffic is one kind's last hour and last day.
type KindTraffic struct {
	Kind string `json:"kind"`
	Hour Window `json:"hour"`
	Day  Window `json:"day"`
	// Series is requests per minute for the last 60 minutes, oldest first.
	Series []int64 `json:"series"`
}

// Snapshot returns every shown kind (TrafficKinds first, in order; others after, sorted).
// since is when counting started, so a rate over a window longer than that is not diluted.
func (t *Traffic) Snapshot(since time.Time) []KindTraffic {
	now := t.now()
	minute := now.Unix() / 60
	up := now.Sub(since).Minutes()
	t.mu.Lock()
	defer t.mu.Unlock()
	names := append([]string(nil), TrafficKinds...)
	var extra []string
	for k := range t.kinds {
		known := false
		for _, n := range TrafficKinds {
			known = known || n == k
		}
		if !known {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)
	out := make([]KindTraffic, 0, len(names))
	for _, name := range names {
		kt := KindTraffic{Kind: name, Series: make([]int64, 60)}
		if k := t.kinds[name]; k != nil {
			kt.Hour = k.window(minute, 60, up)
			kt.Day = k.window(minute, trafficMinutes, up)
			for i := range 60 {
				m := minute - int64(59-i)
				if s := &k.slots[m%trafficMinutes]; s.minute == m {
					kt.Series[i] = int64(s.count)
				}
			}
		}
		out = append(out, kt)
	}
	return out
}

func (k *trafficKind) window(minute int64, span int, upMinutes float64) Window {
	var w Window
	var hist [histBuckets]int64
	for i := range span {
		m := minute - int64(i)
		s := &k.slots[m%trafficMinutes]
		if s.minute != m {
			continue
		}
		w.Requests += int64(s.count)
		w.Errors += int64(s.errors)
		w.Rejected += int64(s.rejected)
		for b, n := range s.hist {
			hist[b] += int64(n)
		}
	}
	over := math.Min(float64(span), math.Max(upMinutes, 1))
	w.PerMin = math.Round(float64(w.Requests)/over*100) / 100
	w.P50ms = quantile(hist[:], w.Requests, 0.50)
	w.P95ms = quantile(hist[:], w.Requests, 0.95)
	return w
}

// quantile returns the upper bound of the bucket holding the q-th request, in milliseconds.
func quantile(hist []int64, total int64, q float64) float64 {
	if total == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	var seen int64
	for i, n := range hist {
		seen += n
		if seen >= rank {
			return math.Round(bucketBound(i)*100) / 100
		}
	}
	return math.Round(bucketBound(len(hist)-1)*100) / 100
}
