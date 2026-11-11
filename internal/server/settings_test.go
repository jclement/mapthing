package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jclement/mapthing/internal/config"
)

func TestSettingsEndpoint(t *testing.T) {
	env := []string{"ADMIN_USER=admin", "ADMIN_PASSWORD=" + adminPass, "MAPTHING_INTERNAL_TOKEN=" + token,
		"MAPTHING_PUBLIC_URL=https://maps.example.com", "MAPTHING_TYPO=1"}
	e := newEnv(t, false, func(d *Deps) {
		d.Settings = func() []config.Setting { return config.Describe(env, nil) }
		d.Versions = func() []Component { return []Component{{Name: "Valhalla", Version: "3.9.1", Detail: "routing"}} }
	})
	if r, _ := e.do(nil, "GET", "/admin/api/settings", nil, nil); r.StatusCode != 401 {
		t.Fatalf("settings without a session: %d", r.StatusCode)
	}
	c := e.login()
	r, b := e.do(c, "GET", "/admin/api/settings", nil, nil)
	if r.StatusCode != 200 {
		t.Fatalf("settings: %d %s", r.StatusCode, b)
	}
	if strings.Contains(string(b), adminPass) || strings.Contains(string(b), token) {
		t.Fatalf("the settings response carries a secret: %s", b)
	}
	var out struct {
		Settings []config.Setting `json:"settings"`
		Versions []Component      `json:"versions"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	by := map[string]config.Setting{}
	for _, s := range out.Settings {
		by[s.Name] = s
	}
	if s := by["ADMIN_PASSWORD"]; !s.Secret || !s.Set || s.Value != config.SecretMask || s.Source != "env" {
		t.Errorf("ADMIN_PASSWORD: %+v", s)
	}
	if s := by["MAPTHING_PUBLIC_URL"]; s.Value != "https://maps.example.com" || s.Source != "env" {
		t.Errorf("MAPTHING_PUBLIC_URL: %+v", s)
	}
	if s := by["MAPTHING_LISTEN"]; s.Value != ":8080" || s.Source != "default" {
		t.Errorf("MAPTHING_LISTEN: %+v", s)
	}
	if s := by["MAPTHING_TYPO"]; s.Group != "Not recognised" {
		t.Errorf("MAPTHING_TYPO: %+v", s)
	}
	names := map[string]string{}
	for _, v := range out.Versions {
		names[v.Name] = v.Version
	}
	if names["mapthing"] == "" || names["Go"] == "" || names["Valhalla"] != "3.9.1" {
		t.Errorf("versions: %+v", out.Versions)
	}
	// Read-only: there is nothing to write to.
	for _, m := range []string{"PUT", "POST", "PATCH", "DELETE"} {
		if r, _ := e.do(c, m, "/admin/api/settings", map[string]any{}, map[string]string{"Origin": e.ts.URL}); r.StatusCode != 404 {
			t.Errorf("%s /admin/api/settings: %d, want 404", m, r.StatusCode)
		}
	}
}
