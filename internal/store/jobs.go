package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// JobRow is a persisted job.
type JobRow struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Params     string     `json:"params"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	Stage      string     `json:"stage"`
	Pct        float64    `json:"pct"`
	Message    string     `json:"message"`
	Error      string     `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Stages     []StageRow `json:"stages"`
	Generation string     `json:"generation"`
}

// StageRow is one stage of a job.
type StageRow struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"` // pending | running | done | failed | skipped
	Progress   float64    `json:"progress"`
	Detail     string     `json:"detail,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// LogLine is one persisted job log line.
type LogLine struct {
	ID    int64     `json:"id"`
	JobID int64     `json:"job_id"`
	At    time.Time `json:"at"`
	Line  string    `json:"line"`
}

const jobCols = `id, name, params, trigger, status, stage, pct, message, error, created_at, started_at, finished_at, stages, generation`

func scanJob(sc interface{ Scan(...any) error }) (*JobRow, error) {
	var j JobRow
	var created int64
	var started, finished sql.NullInt64
	var stages string
	if err := sc.Scan(&j.ID, &j.Name, &j.Params, &j.Trigger, &j.Status, &j.Stage, &j.Pct, &j.Message, &j.Error, &created, &started, &finished, &stages, &j.Generation); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(stages), &j.Stages)
	j.CreatedAt = time.Unix(created, 0).UTC()
	j.StartedAt = nullTime(started)
	j.FinishedAt = nullTime(finished)
	return &j, nil
}

// CreateJob inserts a queued job.
func (s *Store) CreateJob(ctx context.Context, name, params, trigger string) (*JobRow, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO jobs (name, params, trigger, status, created_at) VALUES (?, ?, ?, 'queued', ?)`,
		name, params, trigger, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Job(ctx, id)
}

// Job returns one job.
func (s *Store) Job(ctx context.Context, id int64) (*JobRow, error) {
	j, err := scanJob(s.DB.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Jobs lists the most recent jobs (limit <= 0 means 50).
func (s *Store) Jobs(ctx context.Context, limit int) ([]*JobRow, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryJobs(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY id DESC LIMIT ?`, limit)
}

// JobsByStatus lists jobs in a status, oldest first.
func (s *Store) JobsByStatus(ctx context.Context, status string) ([]*JobRow, error) {
	return s.queryJobs(ctx, `SELECT `+jobCols+` FROM jobs WHERE status = ? ORDER BY id`, status)
}

func (s *Store) queryJobs(ctx context.Context, q string, args ...any) ([]*JobRow, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*JobRow{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateJob writes a job's mutable fields.
func (s *Store) UpdateJob(ctx context.Context, j *JobRow) error {
	stages, _ := json.Marshal(j.Stages)
	if j.Stages == nil {
		stages = []byte("[]")
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE jobs SET status = ?, stage = ?, pct = ?, message = ?, error = ?, started_at = ?, finished_at = ?, stages = ?, generation = ? WHERE id = ?`,
		j.Status, j.Stage, j.Pct, j.Message, j.Error, unixOrNil(j.StartedAt), unixOrNil(j.FinishedAt), string(stages), j.Generation, j.ID)
	return err
}

// AppendJobLog stores a log line.
func (s *Store) AppendJobLog(ctx context.Context, jobID int64, at time.Time, line string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO job_logs (job_id, at, line) VALUES (?, ?, ?)`, jobID, at.UnixMilli(), line)
	return err
}

// JobLogTail returns the last n log lines of a job, oldest first.
func (s *Store) JobLogTail(ctx context.Context, jobID int64, n int) ([]LogLine, error) {
	if n <= 0 {
		n = 200
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, job_id, at, line FROM (SELECT id, job_id, at, line FROM job_logs WHERE job_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, jobID, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LogLine{}
	for rows.Next() {
		var l LogLine
		var at int64
		if err := rows.Scan(&l.ID, &l.JobID, &at, &l.Line); err != nil {
			return nil, err
		}
		l.At = time.UnixMilli(at).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// PruneJobLogs keeps only the logs of the newest keepJobs jobs.
func (s *Store) PruneJobLogs(ctx context.Context, keepJobs int) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM job_logs WHERE job_id NOT IN (SELECT id FROM jobs ORDER BY id DESC LIMIT ?)`, keepJobs)
	return err
}
