package jobs

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jclement/mapthing/internal/store"
)

// JobJSON is a job as the admin API and the SSE stream present it (docs/architecture.md,
// "Admin UI and admin API shapes").
type JobJSON struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Status           string           `json:"status"`
	Trigger          string           `json:"trigger"`
	RefreshDownloads bool             `json:"refresh_downloads"`
	CreatedAt        time.Time        `json:"created_at"`
	StartedAt        *time.Time       `json:"started_at,omitempty"`
	FinishedAt       *time.Time       `json:"finished_at,omitempty"`
	Progress         float64          `json:"progress"`
	Stages           []store.StageRow `json:"stages"`
	Error            string           `json:"error,omitempty"`
	GenerationID     string           `json:"generation_id,omitempty"`
}

// View converts a stored job to its API shape.
func View(r store.JobRow) JobJSON {
	v := JobJSON{
		ID: strconv.FormatInt(r.ID, 10), Name: r.Name, Status: r.Status, Trigger: r.Trigger,
		CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		Stages: r.Stages, Error: r.Error, GenerationID: r.Generation,
	}
	if v.Stages == nil {
		v.Stages = []store.StageRow{}
	}
	var p struct {
		Refresh          bool `json:"refresh"`
		RefreshDownloads bool `json:"refresh_downloads"`
	}
	_ = json.Unmarshal([]byte(r.Params), &p)
	v.RefreshDownloads = p.Refresh || p.RefreshDownloads
	switch {
	case r.Status == StatusSucceeded:
		v.Progress = 1
	case len(r.Stages) > 0:
		var sum float64
		for _, s := range r.Stages {
			switch s.Status {
			case "done", "skipped":
				sum++
			case "running", "failed":
				sum += s.Progress
			}
		}
		v.Progress = sum / float64(len(r.Stages))
	default:
		v.Progress = r.Pct
	}
	return v
}

// LogJSON is a log line as the admin API and SSE present it.
type LogJSON struct {
	JobID string    `json:"job_id"`
	T     time.Time `json:"t"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// LogView converts a stored log line; the level is inferred from the text.
func LogView(l store.LogLine) LogJSON {
	level := "info"
	low := strings.ToLower(l.Line)
	switch {
	case strings.HasPrefix(low, "error") || strings.Contains(low, "job failed") || strings.Contains(low, " error:"):
		level = "error"
	case strings.HasPrefix(low, "warn") || strings.Contains(low, " warning"):
		level = "warn"
	case strings.HasPrefix(low, "debug"):
		level = "debug"
	}
	return LogJSON{JobID: strconv.FormatInt(l.JobID, 10), T: l.At, Level: level, Msg: l.Line}
}
