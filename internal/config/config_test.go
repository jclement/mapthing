package config

import "testing"

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
