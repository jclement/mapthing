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
	// Authorization and friends, whatever the scheme: the whole value to the end of the token.
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|x-goog-api-key|x-mapthing-internal|x-api-key|cookie|set-cookie)\b(\\?["']?\s*[:=]\s*\[?\\?["']?)(?:(?:bearer|basic|digest|token)\s+)?[^\s"',;\]\\]+`), `$1$2` + Mask},
	// A bearer token anywhere.
	{regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`), `$1 ` + Mask},
	// Query parameters that carry a key: ?key=, &api_key=, &token=, &sig=…
	{regexp.MustCompile(`(?i)([?&](?:key|sig|` + secretName + `)=)[^&\s"'<>\\]+`), `$1` + Mask},
	// name=value and "name": "value" where the name says it is a secret.
	{regexp.MustCompile(`(?i)(\b` + secretName + `\b\\?["']?\s*[:=]\s*)` + value), `$1` + Mask},
	// Credentials in a URL: scheme://user:password@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`), `$1` + Mask + `@`},
	// mapthing API keys (mt_ + 52 base32 characters), wherever they appear.
	{regexp.MustCompile(`\bmt_[a-z2-7]{20,}\b`), `mt_` + Mask},
	// Google API keys.
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}`), Mask},
	// JSON Web Tokens and other base64 JSON blobs (a Cloudflare Tunnel token is one).
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{16,}(?:\.[A-Za-z0-9_-]+)*`), Mask},
}

// Redact removes anything that looks like a password, key or token from a log line.
func Redact(s string) string {
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
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
	r.literals = append(r.literals, v)
	// Longest first, so a secret that contains another is removed whole.
	sort.Slice(r.literals, func(i, j int) bool { return len(r.literals[i]) > len(r.literals[j]) })
}

// Redact applies the literal secrets, then the patterns.
func (r *Redactor) Redact(s string) string {
	if r != nil {
		r.mu.RLock()
		for _, lit := range r.literals {
			if strings.Contains(s, lit) {
				s = strings.ReplaceAll(s, lit, Mask)
			}
		}
		r.mu.RUnlock()
	}
	return Redact(s)
}
