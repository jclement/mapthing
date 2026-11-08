package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// WriteError writes a JSON error in the shape the caller's API family expects:
//
//   - Google Elevation (/maps/api/...): {"status":"REQUEST_DENIED","error_message":"...","results":[]}
//   - Google Routes (/directions/v2...): {"error":{"code":403,"message":"...","status":"PERMISSION_DENIED"}}
//   - OpenRouteService (/ors/...): {"error":{"code":<n>,"message":"..."}}
//   - everything else: {"error":{"code":"<code>","message":"..."}}
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	p := ""
	if r != nil {
		p = r.URL.Path
	}
	switch {
	case strings.HasPrefix(p, "/maps/api/"):
		gs := "REQUEST_DENIED"
		switch status {
		case http.StatusTooManyRequests:
			gs = "OVER_QUERY_LIMIT"
		case http.StatusBadRequest:
			gs = "INVALID_REQUEST"
		case http.StatusInternalServerError, http.StatusServiceUnavailable:
			gs = "UNKNOWN_ERROR"
		}
		// Google's Elevation API answers errors with HTTP 200 and a status field for
		// REQUEST_DENIED/INVALID_REQUEST; keep the real HTTP code so caches and logs see it,
		// clients read the status field.
		writeJSON(w, status, map[string]any{"status": gs, "error_message": msg, "results": []any{}})
	case strings.HasPrefix(p, "/directions/v2"):
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": msg, "status": googleStatus(status)}})
	case strings.HasPrefix(p, "/ors/"):
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": msg}})
	default:
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
}

func googleStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
