package config

import (
	"strings"
	"testing"
)

func TestParseServe(t *testing.T) {
	all := Serve{Tiles: true, Routing: true, Elevation: true}
	for in, want := range map[string]Serve{
		"":                          all,
		"all":                       all,
		"tiles,routing,elevation":   all,
		"routing,elevation":         {Routing: true, Elevation: true},
		" Routing , elevation ,":    {Routing: true, Elevation: true},
		"tiles":                     {Tiles: true},
		"elevation,elevation,tiles": {Tiles: true, Elevation: true},
	} {
		got, err := ParseServe(in)
		if err != nil || got != want {
			t.Errorf("ParseServe(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"raster", "routing,elevations", ",", "none"} {
		if _, err := ParseServe(in); err == nil {
			t.Errorf("ParseServe(%q) accepted", in)
		}
	}
	if got := (Serve{Routing: true, Elevation: true}).List(); len(got) != 2 || got[0] != "routing" || got[1] != "elevation" {
		t.Errorf("List %v", got)
	}
}

func TestSlimModeFromEnv(t *testing.T) {
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASSWORD", "x")
	load := func(renderer, serve string) (Config, error) {
		t.Setenv("MAPTHING_RENDERER", renderer)
		t.Setenv("MAPTHING_SERVE", serve)
		return FromEnv()
	}
	if c, err := load("", ""); err != nil || c.RendererOff || !c.Serve.Tiles || !c.Serve.Routing || !c.Serve.Elevation {
		t.Errorf("defaults: %+v %v", c, err)
	}
	if c, err := load("off", ""); err != nil || !c.RendererOff || !c.Serve.Tiles {
		t.Errorf("renderer off: %+v %v", c, err)
	}
	if c, err := load("OFF", ""); err != nil || !c.RendererOff {
		t.Errorf("renderer OFF: %+v %v", c, err)
	}
	// No tiles to render from: the renderer is off whatever MAPTHING_RENDERER says.
	if c, err := load("on", "routing,elevation"); err != nil || !c.RendererOff || c.Serve.Tiles {
		t.Errorf("serve without tiles: %+v %v", c, err)
	}
	if _, err := load("maybe", ""); err == nil {
		t.Errorf("MAPTHING_RENDERER=maybe accepted")
	}
	if _, err := load("", "tiles,maps"); err == nil {
		t.Errorf("MAPTHING_SERVE=tiles,maps accepted")
	}
}

func TestTrustedProxy(t *testing.T) {
	t.Setenv("ADMIN_USER", "a")
	t.Setenv("ADMIN_PASSWORD", "b")
	for v, want := range map[string]bool{"": false, "off": false, "0": false, "on": true, "ON": true, "true": true, "1": true} {
		t.Setenv("MAPTHING_TRUSTED_PROXY", v)
		c, err := FromEnv()
		if err != nil || c.TrustedProxy != want {
			t.Errorf("MAPTHING_TRUSTED_PROXY=%q: %v, %v; want %v", v, c.TrustedProxy, err, want)
		}
	}
	t.Setenv("MAPTHING_TRUSTED_PROXY", "cloudflare")
	if _, err := FromEnv(); err == nil {
		t.Error("a value that is neither on nor off was accepted")
	}
}

func TestDescribe(t *testing.T) {
	env := []string{
		"ADMIN_USER=admin",
		"ADMIN_PASSWORD=correct-horse",
		"MAPTHING_PUBLIC_URL=https://maps.example.com",
		"MAPTHING_IMAGERY_URL=https://tiles.example.com/{z}/{x}/{y}.jpg?key=sekrit",
		"MAPTHING_LOG_MAX_MB=",                         // set but empty: the default applies
		"MAPTHING_RENDERER_CMD=/opt/renderer/start.sh", // what the image sets
		"MAPTHING_LISTEN=:9090",                        // the image sets :8080; this was changed
		"MAPTHING_TRUSTED_PROXI=on",                    // a typo
		"MAPTHING_SPARE_API_KEY=abc123",                // unknown and secret-looking
		"MAPTHING_IMAGE_VERSION=1.2.3",                 // the image's own, not a setting
		"TUNNEL_TOKEN=eyJhbGciOi",                      // not ours at all: never listed
		"PATH=/usr/bin",
	}
	redact := func(s string) string { return strings.ReplaceAll(s, "sekrit", "[redacted]") }
	got := map[string]Setting{}
	var order []string
	for _, s := range Describe(env, redact) {
		got[s.Name] = s
		order = append(order, s.Name)
	}
	check := func(name, value, source string, set bool) {
		t.Helper()
		s, ok := got[name]
		if !ok || s.Value != value || s.Source != source || s.Set != set {
			t.Errorf("%s = %+v (listed %v); want value %q source %q set %v", name, s, ok, value, source, set)
		}
	}
	check("ADMIN_USER", "admin", "env", true)
	check("ADMIN_PASSWORD", SecretMask, "env", true)
	check("MAPTHING_INTERNAL_TOKEN", "", "default", false)
	check("MAPTHING_PUBLIC_URL", "https://maps.example.com", "env", true)
	check("MAPTHING_IMAGERY_URL", "https://tiles.example.com/{z}/{x}/{y}.jpg?key=[redacted]", "env", true)
	check("MAPTHING_LOG_MAX_MB", "50", "default", false)
	check("MAPTHING_TRUSTED_PROXY", "off", "default", false)
	check("MAPTHING_RENDERER_CMD", "/opt/renderer/start.sh", "image", true)
	check("MAPTHING_LISTEN", ":9090", "env", true)
	check("MAPTHING_TRUSTED_PROXI", "on", "env", true)
	check("MAPTHING_SPARE_API_KEY", SecretMask, "env", true)
	if got["MAPTHING_TRUSTED_PROXI"].Group != "Not recognised" {
		t.Errorf("the typo is in group %q", got["MAPTHING_TRUSTED_PROXI"].Group)
	}
	for _, absent := range []string{"MAPTHING_IMAGE_VERSION", "TUNNEL_TOKEN", "PATH"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s should not be listed", absent)
		}
	}
	all := strings.Join(order, ",")
	for _, s := range got {
		if strings.Contains(s.Value, "correct-horse") || strings.Contains(s.Value, "sekrit") || strings.Contains(s.Value, "abc123") || strings.Contains(s.Value, "eyJ") {
			t.Errorf("%s leaks a secret: %q", s.Name, s.Value)
		}
		if s.Help == "" {
			t.Errorf("%s has no help text", s.Name)
		}
	}
	if order[0] != "ADMIN_USER" || !strings.HasSuffix(all, "MAPTHING_SPARE_API_KEY,MAPTHING_TRUSTED_PROXI") {
		t.Errorf("order: %s", all)
	}
}
