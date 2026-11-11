package keys

// Rate limiting.
//
// Every keyed request has a cost: the weight of its kind (Config.Weights; a vector tile 1, a
// raster render or a route 10). A key can have
//
//   - a rate: cost units per second with a burst (a token bucket);
//   - a rate per client address within the key, so one abuser of a key that sits in a
//     browser cannot use it all up;
//   - a daily and a monthly quota of cost units (UTC).
//
// and the server has a global safety limit per expensive kind, in requests per second across
// all keys. A request that fails any of them is answered 429 with Retry-After.
//
// Buckets live in memory and start full after a restart. Quotas are kept in SQLite: used units
// are written every few seconds (Service.Run) and read back the first time a key is seen, so
// a restart loses at most a flush interval of a quota.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

// Scopes: which limit refused a request.
const (
	ScopeRate    = "rate"    // the key's rate
	ScopeIP      = "ip"      // the key's per-client rate
	ScopeDaily   = "daily"   // the key's daily quota
	ScopeMonthly = "monthly" // the key's monthly quota
	ScopeGlobal  = "global"  // the server's limit for this kind of request
)

// GlobalLimit is a server-wide limit for one kind, in requests.
type GlobalLimit struct {
	RPS   float64 `json:"rps"`
	Burst int     `json:"burst"`
}

// Config is the part of rate limiting that is not per key.
type Config struct {
	// Weights is the cost of one request by usage kind. A kind not listed costs 1.
	Weights map[string]int `json:"weights"`
	// Global limits by usage kind, across all keys. RPS 0 = no limit.
	Global map[string]GlobalLimit `json:"global"`
}

// Kinds lists the usage kinds a weight or a global limit can be set for, in display order.
var Kinds = []string{"tile", "raster", "route", "matrix", "isochrone", "snap", "valhalla", "elevation", "pack"}

// DefaultConfig is what a new server uses. The weights follow the measured CPU cost of each
// request (docs/sizing.md: a vector tile 0.5 ms, a route 15 ms, a profile 36 ms, a cold raster
// render 124 ms), coarsely. The global limits sit near what the reference machine served of
// each kind, so they stop a flood without getting in the way of ordinary use.
func DefaultConfig() Config {
	return Config{
		Weights: map[string]int{"tile": 1, "raster": 10, "route": 10, "matrix": 20, "isochrone": 20,
			"snap": 2, "valhalla": 10, "elevation": 5, "pack": 50},
		Global: map[string]GlobalLimit{
			"raster": {RPS: 100, Burst: 200}, "route": {RPS: 60, Burst: 120}, "matrix": {RPS: 10, Burst: 20},
			"isochrone": {RPS: 10, Burst: 20}, "valhalla": {RPS: 60, Burst: 120}, "elevation": {RPS: 100, Burst: 200},
			"pack": {RPS: 2, Burst: 4},
		},
	}
}

// Validate checks a configuration from the admin API.
func (c Config) Validate() error {
	known := map[string]bool{}
	for _, k := range Kinds {
		known[k] = true
	}
	for k, w := range c.Weights {
		if !known[k] {
			return errors.New("unknown request kind " + strconvQuote(k))
		}
		if w < 1 || w > 100000 {
			return errors.New("the weight of " + k + " must be between 1 and 100000")
		}
	}
	for k, g := range c.Global {
		if !known[k] {
			return errors.New("unknown request kind " + strconvQuote(k))
		}
		if g.RPS < 0 || g.Burst < 0 || math.IsNaN(g.RPS) || math.IsInf(g.RPS, 0) {
			return errors.New("the global limit of " + k + " cannot be negative")
		}
	}
	return nil
}

func strconvQuote(s string) string { return `"` + s + `"` }

// withDefaults returns c with a weight for every kind and tidy bursts.
func (c Config) withDefaults() Config {
	out := Config{Weights: map[string]int{}, Global: map[string]GlobalLimit{}}
	def := DefaultConfig()
	for _, k := range Kinds {
		w := c.Weights[k]
		if w <= 0 {
			w = def.Weights[k]
		}
		if w <= 0 {
			w = 1
		}
		out.Weights[k] = w
		g, ok := c.Global[k]
		if !ok && c.Global == nil {
			g = def.Global[k]
		}
		if g.RPS > 0 {
			if g.Burst <= 0 {
				g.Burst = max(1, int(math.Ceil(g.RPS*2)))
			}
			out.Global[k] = g
		}
	}
	return out
}

const configSetting = "limits.config"

// Config returns the current weights and global limits.
func (s *Service) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// SetConfig validates, stores and applies weights and global limits.
func (s *Service) SetConfig(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Global == nil {
		c.Global = map[string]GlobalLimit{} // an explicit "none", not "the defaults"
	}
	c = c.withDefaults()
	b, _ := json.Marshal(c)
	if err := s.st.SetSetting(ctx, configSetting, string(b)); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = c
	s.global = map[string]*bucket{}
	s.mu.Unlock()
	return nil
}

func (s *Service) loadConfig(ctx context.Context) {
	cfg := DefaultConfig()
	if v, err := s.st.Setting(ctx, configSetting); err == nil {
		var stored Config
		if json.Unmarshal([]byte(v), &stored) == nil && stored.Validate() == nil {
			if stored.Global == nil {
				stored.Global = map[string]GlobalLimit{}
			}
			cfg = stored
		}
	}
	s.cfg = cfg.withDefaults()
}

// Cost returns what one request of this kind costs.
func (c Config) Cost(kind string) int {
	if w := c.Weights[kind]; w > 0 {
		return w
	}
	if strings.HasPrefix(kind, "elevation") {
		if w := c.Weights["elevation"]; w > 0 {
			return w
		}
	}
	return 1
}

// Decision is the outcome of charging one request.
type Decision struct {
	OK bool
	// Scope says which limit refused the request (ScopeRate …); "" when it was allowed.
	Scope string
	// RetryAfter is how long until the request would be allowed.
	RetryAfter time.Duration
	// Cost is what the request costs in units.
	Cost int
	// Limit, Remaining and Reset describe the limit closest to running out, for the
	// X-RateLimit-* headers: its size, what is left, and how long until it is whole again.
	// Limit 0 means the key has no limits at all.
	Limit, Remaining int64
	Reset            time.Duration
}

// Err returns the matching error for a refusal (nil when allowed), for callers that only
// need the old behaviour.
func (d Decision) Err() error {
	switch d.Scope {
	case "":
		return nil
	case ScopeDaily:
		return ErrDayLimit
	case ScopeMonthly:
		return ErrMonthLimit
	case ScopeGlobal:
		return ErrBusy
	}
	return ErrRateLimit
}

// Message says what happened, for the error body.
func (d Decision) Message() string {
	switch d.Scope {
	case ScopeRate:
		return "this API key is sending requests faster than its rate limit allows"
	case ScopeIP:
		return "this client is sending requests faster than this API key allows a single address"
	case ScopeDaily:
		return "this API key has used its daily quota; it resets at 00:00 UTC"
	case ScopeMonthly:
		return "this API key has used its monthly quota; it resets on the 1st at 00:00 UTC"
	case ScopeGlobal:
		return "the server is at its limit for this kind of request; try again shortly"
	}
	return ""
}

type bucket struct {
	tokens float64
	last   time.Time
}

// take refills b and takes cost tokens. When there are not enough it takes nothing and
// returns how long until there would be. capacity is at least cost, so a request that costs
// more than the burst is not refused for ever.
func (b *bucket) take(now time.Time, rate float64, capacity, cost float64) (ok bool, wait time.Duration) {
	capacity = math.Max(capacity, cost)
	if b.last.IsZero() {
		b.tokens, b.last = capacity, now
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(capacity, b.tokens+el*rate)
		b.last = now
	}
	if b.tokens > capacity {
		b.tokens = capacity
	}
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0
	}
	if rate <= 0 {
		return false, time.Hour
	}
	return false, time.Duration(math.Ceil((cost-b.tokens)/rate*1000)) * time.Millisecond
}

// refund puts back what take took (a later check refused the request).
func (b *bucket) refund(cost float64) { b.tokens += cost }

type quotaState struct {
	day, month           string
	dayUnits, monthUnits int64 // including what is not flushed yet
	seededDay, seededMon bool
}

type ipKey struct {
	key int64
	ip  string
}

// Refusal is one refused request, kept for the admin UI.
type Refusal struct {
	At    time.Time `json:"t"`
	Kind  string    `json:"kind"`
	Scope string    `json:"scope"`
	IP    string    `json:"ip,omitempty"`
}

const (
	maxIPBuckets = 20000
	ipIdle       = 10 * time.Minute
	keepRefusals = 20
)

// clientNet reduces an address to what a per-client limit should count as one client: an
// IPv4 address, or an IPv6 /64 (one subscriber usually has a whole /64).
func clientNet(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return v4.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Charge charges one request of kind from client address ip against key k: the server's
// global limit for the kind, then the key's rate, its per-client rate and its quotas. Nothing
// is charged when any of them refuses.
func (s *Service) Charge(ctx context.Context, k *store.APIKey, kind, ip string) Decision {
	now := s.now()
	lim := k.Limits.Normalise()
	day := now.UTC().Format("2006-01-02")
	month := day[:7]

	// Units used need what an earlier run of the server already counted. It is read once
	// per key and period, before anything of this run is counted for that period, so the
	// stored figure never includes what is added here.
	s.mu.Lock()
	q := s.quota[k.ID]
	if q == nil {
		q = &quotaState{}
		s.quota[k.ID] = q
	}
	needDay := q.day != day || !q.seededDay
	needMon := q.month != month || !q.seededMon
	s.mu.Unlock()
	var storedDay, storedMon int64
	if needDay {
		storedDay, _ = s.st.QuotaUsed(ctx, k.ID, day)
	}
	if needMon {
		storedMon, _ = s.st.QuotaUsed(ctx, k.ID, month)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	cost := cfg.Cost(kind)
	d := Decision{Cost: cost}
	// Roll the periods over, keeping what is still waiting to be written.
	if q.day != day {
		q.day, q.dayUnits, q.seededDay = day, 0, false
	}
	if q.month != month {
		q.month, q.monthUnits, q.seededMon = month, 0, false
	}
	if needDay && !q.seededDay {
		q.dayUnits += storedDay
		q.seededDay = true
	}
	if needMon && !q.seededMon {
		q.monthUnits += storedMon
		q.seededMon = true
	}

	refuse := func(scope string, wait time.Duration) Decision {
		d.OK, d.Scope, d.RetryAfter = false, scope, max(wait, time.Second)
		s.limited[store.LimitedKey{KeyID: k.ID, Day: day, Scope: scope}]++
		r := append(s.refusals[k.ID], Refusal{At: now.UTC(), Kind: kind, Scope: scope, IP: ip})
		if len(r) > keepRefusals {
			r = r[len(r)-keepRefusals:]
		}
		s.refusals[k.ID] = r
		s.headers(&d, lim, k.ID, q, now)
		return d
	}

	// Quotas first: a key that is out of quota should not also drain its buckets.
	if lim.DailyQuota > 0 && q.dayUnits+int64(cost) > lim.DailyQuota {
		return refuse(ScopeDaily, untilNextDay(now))
	}
	if lim.MonthlyQuota > 0 && q.monthUnits+int64(cost) > lim.MonthlyQuota {
		return refuse(ScopeMonthly, untilNextMonth(now))
	}
	var gb *bucket
	if g, ok := cfg.Global[kind]; ok && g.RPS > 0 {
		gb = s.global[kind]
		if gb == nil {
			gb = &bucket{}
			s.global[kind] = gb
		}
		if ok, wait := gb.take(now, g.RPS, float64(g.Burst), 1); !ok {
			return refuse(ScopeGlobal, wait)
		}
	}
	var kb *bucket
	if lim.RPS > 0 {
		kb = s.buckets[k.ID]
		if kb == nil {
			kb = &bucket{}
			s.buckets[k.ID] = kb
		}
		if ok, wait := kb.take(now, lim.RPS, float64(lim.Burst), float64(cost)); !ok {
			if gb != nil {
				gb.refund(1)
			}
			return refuse(ScopeRate, wait)
		}
	}
	if lim.IPRPS > 0 && ip != "" {
		ik := ipKey{k.ID, clientNet(ip)}
		ib := s.ipBuckets[ik]
		if ib == nil {
			s.pruneIPLocked(now)
			ib = &bucket{}
			s.ipBuckets[ik] = ib
		}
		if ok, wait := ib.take(now, lim.IPRPS, float64(lim.IPBurst), float64(cost)); !ok {
			if gb != nil {
				gb.refund(1)
			}
			if kb != nil {
				kb.refund(float64(cost))
			}
			return refuse(ScopeIP, wait)
		}
	}
	q.dayUnits += int64(cost)
	q.monthUnits += int64(cost)
	s.pending[store.QuotaKey{KeyID: k.ID, Period: day}] += int64(cost)
	s.pending[store.QuotaKey{KeyID: k.ID, Period: month}] += int64(cost)
	d.OK = true
	s.headers(&d, lim, k.ID, q, now)
	return d
}

// headers fills in the limit closest to running out.
func (s *Service) headers(d *Decision, lim store.Limits, id int64, q *quotaState, now time.Time) {
	type cand struct {
		limit, remaining int64
		reset            time.Duration
	}
	var cs []cand
	if lim.RPS > 0 {
		tokens := float64(lim.Burst)
		if b := s.buckets[id]; b != nil && !b.last.IsZero() {
			tokens = math.Min(float64(lim.Burst), b.tokens+now.Sub(b.last).Seconds()*lim.RPS)
		}
		tokens = math.Max(tokens, 0)
		cs = append(cs, cand{int64(lim.Burst), int64(tokens),
			time.Duration(math.Ceil((float64(lim.Burst)-tokens)/lim.RPS)) * time.Second})
	}
	if lim.DailyQuota > 0 {
		cs = append(cs, cand{lim.DailyQuota, max(lim.DailyQuota-q.dayUnits, 0), untilNextDay(now)})
	}
	if lim.MonthlyQuota > 0 {
		cs = append(cs, cand{lim.MonthlyQuota, max(lim.MonthlyQuota-q.monthUnits, 0), untilNextMonth(now)})
	}
	if len(cs) == 0 {
		return
	}
	// Closest to running out: the smallest share left.
	sort.SliceStable(cs, func(i, j int) bool {
		return float64(cs[i].remaining)/float64(cs[i].limit) < float64(cs[j].remaining)/float64(cs[j].limit)
	})
	d.Limit, d.Remaining, d.Reset = cs[0].limit, cs[0].remaining, cs[0].reset
}

func untilNextDay(now time.Time) time.Duration {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC).Sub(u)
}

func untilNextMonth(now time.Time) time.Duration {
	u := now.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(u)
}

// pruneIPLocked forgets per-client buckets that have been idle, and when there are still too
// many, the oldest. Called before adding one.
func (s *Service) pruneIPLocked(now time.Time) {
	if len(s.ipBuckets) < maxIPBuckets {
		return
	}
	for k, b := range s.ipBuckets {
		if now.Sub(b.last) > ipIdle {
			delete(s.ipBuckets, k)
		}
	}
	for k := range s.ipBuckets { // still full: make room (map order is as good as random)
		if len(s.ipBuckets) < maxIPBuckets*9/10 {
			break
		}
		delete(s.ipBuckets, k)
	}
}

// Flush writes used units and refusal counts to the database. On failure they are kept.
func (s *Service) Flush(ctx context.Context) error {
	s.mu.Lock()
	units, limited := s.pending, s.limited
	s.pending, s.limited = map[store.QuotaKey]int64{}, map[store.LimitedKey]int64{}
	s.mu.Unlock()
	if err := s.st.AddQuota(ctx, units, limited); err != nil {
		s.mu.Lock()
		for k, v := range units {
			s.pending[k] += v
		}
		for k, v := range limited {
			s.limited[k] += v
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

// Run flushes every interval until ctx is done, then once more; and once a day drops old rows.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.Flush(fctx); err != nil {
				slog.Warn("rate limits: final flush", "err", err)
			}
			cancel()
			return
		case <-t.C:
			if err := s.Flush(ctx); err != nil {
				slog.Warn("rate limits: flush", "err", err)
			}
			if now := s.now(); now.Sub(lastPrune) > 24*time.Hour {
				lastPrune = now
				if err := s.st.PruneQuota(ctx, now, 400); err != nil {
					slog.Warn("rate limits: prune", "err", err)
				}
			}
		}
	}
}

// Usage is what a key has used against its limits, for the admin UI.
type Usage struct {
	UnitsToday int64     `json:"units_today"`
	UnitsMonth int64     `json:"units_month"`
	Limited    []Refusal `json:"recent_limited"` // newest last
	// RateRemaining is what is left in the key's bucket now (-1 when it has no rate limit).
	RateRemaining int64 `json:"rate_remaining"`
}

// UsageOf reports a key's use in the current day and month and its recent refusals.
func (s *Service) UsageOf(ctx context.Context, k *store.APIKey) Usage {
	now := s.now()
	day := now.UTC().Format("2006-01-02")
	month := day[:7]
	storedDay, _ := s.st.QuotaUsed(ctx, k.ID, day)
	storedMon, _ := s.st.QuotaUsed(ctx, k.ID, month)
	s.mu.Lock()
	defer s.mu.Unlock()
	u := Usage{
		UnitsToday:    storedDay + s.pending[store.QuotaKey{KeyID: k.ID, Period: day}],
		UnitsMonth:    storedMon + s.pending[store.QuotaKey{KeyID: k.ID, Period: month}],
		Limited:       append([]Refusal{}, s.refusals[k.ID]...),
		RateRemaining: -1,
	}
	if lim := k.Limits.Normalise(); lim.RPS > 0 {
		tokens := float64(lim.Burst)
		if b := s.buckets[k.ID]; b != nil && !b.last.IsZero() {
			tokens = math.Min(tokens, b.tokens+now.Sub(b.last).Seconds()*lim.RPS)
		}
		u.RateRemaining = int64(math.Max(tokens, 0))
	}
	return u
}

// PendingLimited returns refusal counts not written yet (to merge into reports).
func (s *Service) PendingLimited() map[store.LimitedKey]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[store.LimitedKey]int64, len(s.limited))
	for k, v := range s.limited {
		out[k] = v
	}
	return out
}
