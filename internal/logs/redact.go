package logs

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Mask replaces a secret in a log line.
const Mask = "[redacted]"

// secretName matches the name of a setting, header or parameter whose value is a secret:
// anything with password, secret, token, an API key or a signature in it (ADMIN_PASSWORD,
// TUNNEL_TOKEN, internal_token, api_key, X-Goog-Api-Key, access_key, sig).
//
//nolint:gosec // a pattern for names of secrets, not a secret
const secretName = `[A-Za-z0-9_.-]*(?:passw(?:or)?d|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|signature)[A-Za-z0-9_.-]*`

// value is one value after a name: a quoted string or a run without separators.
const value = `(?:\\"[^"\\]*\\"|"(?:[^"\\]|\\.)*"|'[^']*'|[^\s"',&;)}\]\\]+)`

var redactors = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization, Cookie and friends: the whole value, whatever it holds (a scheme and a
	// token, Digest fields, several cookies). A quoted or bracketed value ends at its closing
	// quote or bracket; anything else runs to the end of the line.
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|cookie|set-cookie)\b(\\?["']?\s*[:=]\s*)(?:\[[^\]\r\n]*\]|\\"(?:[^"\\\r\n]|\\[^"])*\\"|"(?:[^"\\\r\n]|\\.)*"|'[^'\r\n]*'|[^\r\n]*)`), `$1$2` + Mask},
	// Single-token key headers.
	{regexp.MustCompile(`(?i)\b(x-goog-api-key|x-mapthing-internal|x-api-key)\b(\\?["']?\s*[:=]\s*\[?\\?["']?)[^\s"',;\]\\]+`), `$1$2` + Mask},
	// A bearer token anywhere.
	{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`), `$1 ` + Mask},
	// Query parameters that carry a key: ?key=, &api_key=, &token=, &sig=…
	{regexp.MustCompile(`(?i)([?&](?:key|sig|` + secretName + `)=)[^&\s"'<>\\]+`), `$1` + Mask},
	// name=value and "name": "value" where the name says it is a secret.
	{regexp.MustCompile(`(?i)(\b` + secretName + `\b\\?["']?\s*[:=]\s*)` + value), `$1` + Mask},
	// Credentials in a URL: scheme://user:password@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`), `$1` + Mask + `@`},
	// mapthing API keys (mt_ + 52 base32 characters), wherever they appear. A shorter run is
	// masked too: a key cut short by a truncated line must not show its beginning.
	{regexp.MustCompile(`\bmt_[a-z2-7]{8,}`), `mt_` + Mask},
	// Google API keys.
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{16,}`), Mask},
	// JSON Web Tokens and other base64 JSON blobs (a Cloudflare Tunnel token is one).
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{16,}(?:\.[A-Za-z0-9_-]+)*`), Mask},
}

func redactPatterns(s string) string {
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Redact removes anything that looks like a password, key or token from a log line. A line
// that hides one behind percent-encoding (%6bey=…, an encoded header dump) is shown decoded,
// with the secret masked.
func Redact(s string) string { return redactWith(s, nil) }

// redactWith masks literals, then patterns, then looks behind percent-encoding: the text is
// decoded (up to three layers) and, when the decoded form holds something the plain form did
// not show, the decoded and masked form is what is kept.
func redactWith(s string, literals []string) string {
	pass := func(t string) string {
		for _, lit := range literals {
			if strings.Contains(t, lit) {
				t = strings.ReplaceAll(t, lit, Mask)
			}
		}
		return redactPatterns(t)
	}
	out := pass(s)
	cur := out
	for range 3 {
		dec, changed := percentDecode(cur)
		if !changed {
			break
		}
		// The output only changes when this layer held a secret; otherwise keep the line as
		// it was written and look one layer further down.
		if red := pass(dec); red != dec {
			out, cur = red, red
		} else {
			cur = dec
		}
	}
	return out
}

// percentDecode replaces every %XX with its byte ('+' is left alone: it is ordinary text far
// more often than an encoded space). changed is false when there was nothing to decode.
func percentDecode(s string) (string, bool) {
	i := strings.IndexByte(s, '%')
	if i < 0 {
		return s, false
	}
	var b strings.Builder
	changed := false
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			c := unhex(s[i+1])<<4 | unhex(s[i+2])
			// Control bytes would break the one-line-per-entry rule; keep those encoded.
			if c >= 0x20 && c != 0x7f {
				b.WriteByte(c)
				i += 2
				changed = true
				continue
			}
		}
		b.WriteByte(s[i])
	}
	if !changed {
		return s, false
	}
	return strings.ToValidUTF8(b.String(), "?"), true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// Redactor is Redact plus the literal secrets this process knows (the admin password, the
// internal token), which are removed wherever they appear, whatever surrounds them.
type Redactor struct {
	mu       sync.RWMutex
	literals []string
}

// AddSecret registers a literal value that must never be logged. Values shorter than 6
// characters are ignored: replacing them would shred ordinary text.
func (r *Redactor) AddSecret(v string) {
	v = strings.TrimSpace(v)
	if len(v) < 6 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, have := range r.literals {
		if have == v {
			return
		}
	}
	// Copy on write: Redact reads the slice without holding the lock for the whole pass.
	r.literals = append(append([]string(nil), r.literals...), v)
	// Longest first, so a secret that contains another is removed whole.
	sort.Slice(r.literals, func(i, j int) bool { return len(r.literals[i]) > len(r.literals[j]) })
}

// Redact applies the literal secrets and the patterns, also behind percent-encoding. A nil
// Redactor applies the patterns alone.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return Redact(s)
	}
	r.mu.RLock()
	lits := r.literals
	r.mu.RUnlock()
	return redactWith(s, lits)
}
