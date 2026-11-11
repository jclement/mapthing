package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jclement/mapthing/internal/scheduler"
)

type refreshResp struct {
	ID            string `json:"id"`
	Refresh       bool   `json:"refresh"`
	RefreshPolicy struct {
		Mode        string `json:"mode"`
		Cron        string `json:"cron"`
		Window      string `json:"window"`
		OnlyChanged bool   `json:"only_changed"`
		Effective   struct {
			Cron        *string `json:"cron"`
			Window      string  `json:"window"`
			OnlyChanged bool    `json:"only_changed"`
			Inherited   bool    `json:"inherited"`
		} `json:"effective"`
		NextDue       *time.Time `json:"next_due"`
		NextBuild     *time.Time `json:"next_build"`
		LastCheckedAt *time.Time `json:"last_checked_at"`
		LastCheck     string     `json:"last_check"`
		LastDetail    string     `json:"last_check_detail"`
	} `json:"refresh_policy"`
	RefreshCost struct {
		DownloadBytes int64    `json:"download_bytes"`
		BuildSeconds  *float64 `json:"build_seconds"`
	} `json:"refresh_cost"`
}

type sourceChecker map[string]any // id → bool (changed) or error

func (c sourceChecker) CheckSource(_ context.Context, id string) (scheduler.SourceCheck, error) {
	switch v := c[id].(type) {
	case error:
		return scheduler.SourceCheck{}, v
	case bool:
		return scheduler.SourceCheck{Changed: v, Detail: "sequence 4330"}, nil
	}
	return scheduler.SourceCheck{}, nil
}

func TestRegionRefreshPolicyAPI(t *testing.T) {
	e := newEnv(t, true, func(d *Deps) {
		d.Scheduler.Checker = sourceChecker{"us/delaware": true, "custom/box": errors.New("geofabrik unreachable")}
	})
	c := e.login()
	o := map[string]string{"Origin": e.ts.URL}
	for _, body := range []map[string]any{{"id": "us/delaware"}, {"name": "Box", "bbox": []float64{-104, 31, -101, 33}}} {
		if r, b := e.do(c, "POST", "/admin/api/regions", body, o); r.StatusCode != 201 {
			t.Fatalf("add region: %d %s", r.StatusCode, b)
		}
	}
	get := func(id string) refreshResp {
		t.Helper()
		r, b := e.do(c, "GET", "/admin/api/regions/"+id, nil, nil)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", id, r.StatusCode, b)
		}
		var out refreshResp
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	patch := func(id string, policy map[string]any) (int, refreshResp, string) {
		t.Helper()
		r, b := e.do(c, "PATCH", "/admin/api/regions/"+id, map[string]any{"refresh_policy": policy}, o)
		var out refreshResp
		_ = json.Unmarshal(b, &out)
		return r.StatusCode, out, string(b)
	}

	// A new region follows the default schedule (weekly, Sunday 03:00 UTC).
	d := get("us/delaware")
	p := d.RefreshPolicy
	if p.Mode != "default" || !p.Effective.Inherited || p.Effective.Cron == nil || *p.Effective.Cron != "0 3 * * 0" || !p.Effective.OnlyChanged ||
		p.NextDue == nil || p.NextDue.Weekday() != time.Sunday || p.NextDue.Hour() != 3 || !d.Refresh {
		t.Fatalf("new region: %+v", d)
	}
	// The build cost is the live generation's build without its terrain (12.5 s of basemap in the fixture).
	if d.RefreshCost.BuildSeconds == nil || *d.RefreshCost.BuildSeconds != 12.5 {
		t.Errorf("refresh cost: %+v", d.RefreshCost)
	}

	// Daily, at night only, whether or not the source changed.
	code, d, body := patch("us/delaware", map[string]any{"mode": "daily", "window": "22:00-06:00", "only_changed": false})
	p = d.RefreshPolicy
	if code != 200 || p.Mode != "daily" || p.Window != "22:00-06:00" || p.OnlyChanged || p.Effective.Inherited ||
		*p.Effective.Cron != "0 3 * * *" || p.Effective.Window != "22:00-06:00" || p.NextBuild == nil {
		t.Fatalf("daily: %d %s", code, body)
	}
	// A cron expression of its own.
	code, d, body = patch("us/delaware", map[string]any{"mode": "cron", "cron": "0 2 * * 1-5"})
	if code != 200 || d.RefreshPolicy.Cron != "0 2 * * 1-5" || *d.RefreshPolicy.Effective.Cron != "0 2 * * 1-5" || !d.RefreshPolicy.OnlyChanged {
		t.Fatalf("cron: %d %s", code, body)
	}
	// Never: no next refresh, and the old boolean follows.
	code, d, body = patch("us/delaware", map[string]any{"mode": "never"})
	if code != 200 || d.Refresh || d.RefreshPolicy.Effective.Cron != nil || d.RefreshPolicy.NextDue != nil {
		t.Fatalf("never: %d %s", code, body)
	}
	// Refusals change nothing.
	for name, bad := range map[string]map[string]any{
		"unknown mode":      {"mode": "hourly"},
		"cron without expr": {"mode": "cron"},
		"bad cron":          {"mode": "cron", "cron": "every tuesday"},
		"bad window":        {"mode": "weekly", "window": "night"},
	} {
		if code, _, body := patch("us/delaware", bad); code != 400 {
			t.Errorf("%s: %d %s, want 400", name, code, body)
		}
	}
	// An expression that never occurs, and a time zone prefix (schedules are UTC only).
	for _, expr := range []string{"0 0 30 2 *", "TZ=America/Edmonton 0 3 * * *"} {
		if code, _, body := patch("us/delaware", map[string]any{"mode": "cron", "cron": expr}); code != 400 {
			t.Errorf("cron %q: %d %s, want 400", expr, code, body)
		}
		if r, b := e.do(c, "PUT", "/admin/api/schedule", map[string]any{"cron": expr}, o); r.StatusCode != 400 {
			t.Errorf("default schedule %q: %d %s, want 400", expr, r.StatusCode, b)
		}
	}
	if d := get("us/delaware"); d.RefreshPolicy.Mode != "never" {
		t.Errorf("a refused change was applied: %+v", d.RefreshPolicy)
	}
	if code, _, _ := patch("no/such", map[string]any{"mode": "daily"}); code != 404 {
		t.Errorf("unknown region: %d", code)
	}
	// The older switch still works.
	if r, b := e.do(c, "PATCH", "/admin/api/regions/us%2Fdelaware", map[string]any{"refresh": true}, o); r.StatusCode != 200 {
		t.Fatalf("legacy patch: %d %s", r.StatusCode, b)
	}
	if d := get("us/delaware"); d.RefreshPolicy.Mode != "default" || !d.Refresh {
		t.Errorf("legacy switch on: %+v", d.RefreshPolicy)
	}

	// The default schedule's window and only-changed apply to regions that follow it.
	r, b := e.do(c, "PUT", "/admin/api/schedule", map[string]any{"window": "01:00-05:00", "only_changed": false}, o)
	var sched struct {
		Window      string `json:"window"`
		OnlyChanged bool   `json:"only_changed"`
		Cron        string `json:"cron"`
	}
	_ = json.Unmarshal(b, &sched)
	if r.StatusCode != 200 || sched.Window != "01:00-05:00" || sched.OnlyChanged || sched.Cron != "0 3 * * 0" {
		t.Fatalf("schedule defaults: %d %s", r.StatusCode, b)
	}
	if d := get("us/delaware"); d.RefreshPolicy.Effective.Window != "01:00-05:00" || d.RefreshPolicy.Effective.OnlyChanged {
		t.Errorf("follower after the default changed: %+v", d.RefreshPolicy.Effective)
	}
	if r, _ := e.do(c, "PUT", "/admin/api/schedule", map[string]any{"window": "sometime"}, o); r.StatusCode != 400 {
		t.Errorf("bad default window: %d", r.StatusCode)
	}
	// Saving the schedule with the older per-region list does not reset a region's own mode.
	patch("us/delaware", map[string]any{"mode": "monthly"})
	e.do(c, "PUT", "/admin/api/schedule", map[string]any{"regions": []map[string]any{{"id": "us/delaware", "enabled": true}}}, o)
	if d := get("us/delaware"); d.RefreshPolicy.Mode != "monthly" {
		t.Errorf("saving the schedule reset the region's mode to %q", d.RefreshPolicy.Mode)
	}

	// Check now: looks at each source, records it, builds nothing.
	r, b = e.do(c, "POST", "/admin/api/regions/check", nil, o)
	if r.StatusCode != 200 {
		t.Fatalf("check: %d %s", r.StatusCode, b)
	}
	if d := get("us/delaware"); d.RefreshPolicy.LastCheck != "changed" || d.RefreshPolicy.LastCheckedAt == nil || d.RefreshPolicy.LastDetail != "sequence 4330" {
		t.Errorf("after check: %+v", d.RefreshPolicy)
	}
	if d := get("custom/box"); d.RefreshPolicy.LastCheck != "error" || d.RefreshPolicy.LastDetail != "geofabrik unreachable" {
		t.Errorf("failed check: %+v", d.RefreshPolicy)
	}
	if js, _ := e.st.Jobs(context.Background(), 5); len(js) != 0 {
		t.Errorf("a check queued %d job(s)", len(js))
	}
	// A second click straight away answers with the same results and says it did not ask again.
	r, _ = e.do(c, "POST", "/admin/api/regions/check", nil, o)
	if r.StatusCode != 200 || r.Header.Get("X-Mapthing-Check") != "recent" {
		t.Errorf("second check: %d, X-Mapthing-Check %q", r.StatusCode, r.Header.Get("X-Mapthing-Check"))
	}
	if r, _ := e.do(nil, "POST", "/admin/api/regions/check", nil, o); r.StatusCode != 401 {
		t.Errorf("check without a session: %d", r.StatusCode)
	}
}
