package tiles

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	rangeRE  = regexp.MustCompile(`^[0-9]{1,5}-[0-9]{1,5}\.pbf$`)
	spriteRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}(@2x)?\.(json|png)$`)
)

// StyleInfo describes one available style.
type StyleInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Dark        bool   `json:"dark"`
}

// Styles lists the embedded styles, from their "mapthing:name", "mapthing:description" and
// optional "mapthing:dark" metadata (dark defaults to an ID ending in "-dark").
func Styles(assets fs.FS) []StyleInfo {
	out := []StyleInfo{}
	if assets == nil {
		return out
	}
	files, _ := fs.Glob(assets, "styles/*.json")
	for _, f := range files {
		id := strings.TrimSuffix(path.Base(f), ".json")
		if !nameRE.MatchString(id) {
			continue
		}
		info := StyleInfo{ID: id, Name: id, Dark: strings.HasSuffix(id, "-dark")}
		if b, err := fs.ReadFile(assets, f); err == nil {
			if !isGLStyle(b) {
				continue // package.json and other non-style JSON
			}
			var st struct {
				Name     string         `json:"name"`
				Metadata map[string]any `json:"metadata"`
			}
			if json.Unmarshal(b, &st) == nil {
				if st.Name != "" {
					info.Name = st.Name
				}
				if v, ok := st.Metadata["mapthing:name"].(string); ok && v != "" {
					info.Name = v
				}
				if v, ok := st.Metadata["mapthing:description"].(string); ok {
					info.Description = v
				}
				if v, ok := st.Metadata["mapthing:dark"].(bool); ok {
					info.Dark = v
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// isGLStyle reports whether b is a MapLibre/Mapbox GL style document (version 8 with layers).
func isGLStyle(b []byte) bool {
	var st struct {
		Version int               `json:"version"`
		Layers  []json.RawMessage `json:"layers"`
	}
	return json.Unmarshal(b, &st) == nil && st.Version == 8 && st.Layers != nil
}

// HasStyle reports whether an embedded style exists.
func (s *Server) HasStyle(name string) bool {
	if !nameRE.MatchString(name) || s.opt.Assets == nil {
		return false
	}
	s.styleOnce.Do(func() {
		s.styleSet = map[string]bool{}
		for _, st := range Styles(s.opt.Assets) {
			s.styleSet[st.ID] = true
		}
	})
	return s.styleSet[name]
}

func (s *Server) handleStyleList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, Styles(s.opt.Assets))
}

// handleStyle serves /styles/{name}.json and the Mapbox-shaped /styles/v1/{user}/{name}.
// "{{BASE_URL}}" and "{{KEY_QS}}" in the style are replaced for the caller.
func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		var ok bool
		if name, ok = strings.CutSuffix(r.PathValue("file"), ".json"); !ok {
			jsonError(w, r, http.StatusNotFound, "not_found", "unknown style")
			return
		}
	}
	if !s.HasStyle(name) {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown style")
		return
	}
	b, err := fs.ReadFile(s.opt.Assets, "styles/"+name+".json")
	if err != nil {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown style")
		return
	}
	qs := keyQS(r)
	b = bytes.ReplaceAll(b, []byte("{{BASE_URL}}"), []byte(s.baseURL(r)))
	b = bytes.ReplaceAll(b, []byte("{{KEY_QS}}"), []byte(qs))
	b = s.patchStyle(b, qs)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(b)
}

// patchStyle appends the key query to the sprite URL (GL clients insert "@2x.json" before the
// query) and, with an imagery override, sets the imagery source's attribution.
func (s *Server) patchStyle(b []byte, qs string) []byte {
	needSprite := qs != "" && bytes.Contains(b, []byte(`"sprite"`))
	if !needSprite && !s.opt.ImageryOverride {
		return b
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var st map[string]any
	if dec.Decode(&st) != nil {
		return b
	}
	changed := false
	if sp, ok := st["sprite"].(string); ok && needSprite && !strings.Contains(sp, "key=") && !strings.Contains(sp, "?") {
		st["sprite"] = sp + qs
		changed = true
	}
	if s.opt.ImageryOverride {
		if srcs, ok := st["sources"].(map[string]any); ok {
			if img, ok := srcs["imagery"].(map[string]any); ok {
				img["attribution"] = ImageryAttribution()
				changed = true
			}
		}
	}
	if !changed {
		return b
	}
	out, err := json.Marshal(st)
	if err != nil {
		return b
	}
	return out
}

// handleGlyphs serves /fonts/{fontstack}/{range}.pbf from assets/fonts/<font>/<range>.pbf,
// trying each font of a comma-separated stack in order; 404 when none has the range.
func (s *Server) handleGlyphs(w http.ResponseWriter, r *http.Request) {
	rng := r.PathValue("range")
	if !rangeRE.MatchString(rng) || s.opt.Assets == nil {
		jsonError(w, r, http.StatusBadRequest, "bad_request", "invalid glyph range")
		return
	}
	for _, font := range strings.Split(r.PathValue("fontstack"), ",") {
		font = strings.TrimSpace(font)
		if font == "" || strings.ContainsAny(font, "/\\") || strings.Contains(font, "..") {
			continue
		}
		if b, err := fs.ReadFile(s.opt.Assets, "fonts/"+font+"/"+rng); err == nil {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(b)
			return
		}
	}
	jsonError(w, r, http.StatusNotFound, "not_found", "glyph range not found")
}

// handleSprite serves /sprites/{name}[@2x].{json,png}.
func (s *Server) handleSprite(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !spriteRE.MatchString(file) || s.opt.Assets == nil {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown sprite")
		return
	}
	b, err := fs.ReadFile(s.opt.Assets, "sprites/"+file)
	if err != nil {
		jsonError(w, r, http.StatusNotFound, "not_found", "unknown sprite")
		return
	}
	ct := "application/json"
	if strings.HasSuffix(file, ".png") {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}
