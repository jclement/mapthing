package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(remote string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://maps.internal:8080/x", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name  string
		trust bool
		hdr   map[string]string
		want  string
	}{
		{"direct", false, nil, "172.18.0.3"},
		{"untrusted headers are ignored", false, map[string]string{"CF-Connecting-IP": "203.0.113.9", "X-Forwarded-For": "198.51.100.7"}, "172.18.0.3"},
		{"cloudflare header", true, map[string]string{"CF-Connecting-IP": "203.0.113.9", "X-Forwarded-For": "198.51.100.7, 172.18.0.3"}, "203.0.113.9"},
		{"first forwarded-for entry", true, map[string]string{"X-Forwarded-For": " 198.51.100.7 , 10.0.0.1, 172.18.0.3"}, "198.51.100.7"},
		{"ipv6", true, map[string]string{"CF-Connecting-IP": "2001:DB8::1"}, "2001:db8::1"},
		{"forwarded-for with a port", true, map[string]string{"X-Forwarded-For": "198.51.100.7:51234"}, "198.51.100.7"},
		{"bracketed ipv6 with a port", true, map[string]string{"X-Forwarded-For": "[2001:db8::2]:443"}, "2001:db8::2"},
		{"junk falls through to the next source", true, map[string]string{"CF-Connecting-IP": "not-an-ip", "X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
		{"junk everywhere falls back to the peer", true, map[string]string{"CF-Connecting-IP": "<script>", "X-Forwarded-For": "unknown"}, "172.18.0.3"},
		{"trusted but no headers", true, nil, "172.18.0.3"},
	}
	for _, c := range cases {
		a := &Auth{trustProxy: c.trust}
		if got := a.ClientIP(req("172.18.0.3:40112", c.hdr)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The renderer's internal token is only accepted from the connection's own loopback peer.
// A forwarded address proves nothing, with or without a trusted proxy.
func TestInternalNeedsALoopbackPeer(t *testing.T) {
	for _, trust := range []bool{false, true} {
		a := &Auth{trustProxy: trust}
		a.SetInternalToken("tok")
		spoof := map[string]string{InternalHeader: "tok", "CF-Connecting-IP": "127.0.0.1", "X-Forwarded-For": "127.0.0.1"}
		if a.isInternal(req("172.18.0.3:40112", spoof)) {
			t.Errorf("trust=%v: a remote peer claiming 127.0.0.1 was taken for the renderer", trust)
		}
		if !a.isInternal(req("127.0.0.1:40112", map[string]string{InternalHeader: "tok"})) {
			t.Errorf("trust=%v: the local renderer was refused", trust)
		}
		// Through the proxy the renderer's own requests still come straight from loopback,
		// whatever a forwarded header says.
		if !a.isInternal(req("127.0.0.1:40112", map[string]string{InternalHeader: "tok", "X-Forwarded-For": "203.0.113.9"})) {
			t.Errorf("trust=%v: loopback peer with a forwarded header was refused", trust)
		}
		if a.isInternal(req("127.0.0.1:40112", map[string]string{InternalHeader: "wrong"})) {
			t.Errorf("trust=%v: wrong token accepted", trust)
		}
	}
}

func TestIsHTTPS(t *testing.T) {
	cases := []struct {
		name   string
		public string
		host   string
		tls    bool
		hdr    map[string]string
		want   bool
	}{
		{"plain http on the box", "https://maps.example.com", "127.0.0.1:8080", false, nil, false},
		{"tls", "", "maps.example.com", true, nil, true},
		{"proxy says https", "", "maps.example.com", false, map[string]string{"X-Forwarded-Proto": "https"}, true},
		{"proxy chain", "", "maps.example.com", false, map[string]string{"X-Forwarded-Proto": "https, http"}, true},
		{"proxy says http", "", "maps.example.com", false, map[string]string{"X-Forwarded-Proto": "http"}, false},
		{"cloudflare visitor", "", "maps.example.com", false, map[string]string{"CF-Visitor": `{"scheme":"https"}`}, true},
		{"public url host, no proto header (a tunnel)", "https://maps.example.com", "maps.example.com", false, nil, true},
		{"public url host via forwarded host", "https://maps.example.com", "mapthing:8080", false, map[string]string{"X-Forwarded-Host": "maps.example.com"}, true},
		{"http public url", "http://maps.example.com", "maps.example.com", false, nil, false},
		{"another host", "https://maps.example.com", "other.example.com", false, nil, false},
	}
	for _, c := range cases {
		a := &Auth{publicURL: c.public}
		r := req("172.18.0.3:1", c.hdr)
		r.Host = c.host
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := a.isHTTPS(r); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
