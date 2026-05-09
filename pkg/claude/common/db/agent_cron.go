package db

import (
	"database/sql"
	"errors"
	"time"
)

// AgentCronJob is a row in agent_cron_jobs. Recurring scheduled
// task that the agentd scheduler fires on a wall-clock interval.
type AgentCronJob struct {
	ID              int64
	Name            string
	OwnerConv       string
	TargetConv      string
	GroupID         int64 // 0 → solo (direct send-keys), >0 → enqueue agent_messages
	IntervalSeconds int64
	Subject         string
	Body            string
	Enabled         bool
	CreatedAt       time.Time
	LastRunAt       time.Time // zero value → "never run, due immediately"
	LastRunStatus   string
}

// InsertAgentCronJob writes a new job row. Returns the new ID.
// CreatedAt is stamped server-side; the caller's value is ignored.
func InsertAgentCronJob(j *AgentCronJob) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := d.Exec(`INSERT INTO agent_cron_jobs
		(name, owner_conv, target_conv, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '')`,
		j.Name, j.OwnerConv, j.TargetConv, j.GroupID, j.IntervalSeconds,
		j.Subject, j.Body, boolToInt(j.Enabled), now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetAgentCronJob returns a single job by ID, or nil if not found.
func GetAgentCronJob(id int64) (*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id, name, owner_conv, target_conv, group_id,
		interval_seconds, subject, body, enabled, created_at,
		last_run_at, last_run_status
		FROM agent_cron_jobs WHERE id = ?`, id)
	j, err := scanAgentCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ListAgentCronJobs returns every job, ordered by ID asc.
func ListAgentCronJobs() ([]*AgentCronJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, name, owner_conv, target_conv, group_id,
		interval_seconds, subject, body, enabled, created_at,
		last_run_at, last_run_status
		FROM agent_cron_jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentCronJob
	for rows.Next() {
		j, err := scanAgentCronJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListDueAgentCronJobs returns enabled jobs whose next-fire time
// has passed (now >= last_run_at + interval). Jobs that have never
// run (last_run_at empty) are always due.
func ListDueAgentCronJobs(now time.Time) ([]*AgentCronJob, error) {
	jobs, err := ListAgentCronJobs()
	if err != nil {
		return nil, err
	}
	out := make([]*AgentCronJob, 0, len(jobs))
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if j.LastRunAt.IsZero() {
			out = append(out, j)
			continue
		}
		if now.Sub(j.LastRunAt) >= time.Duration(j.IntervalSeconds)*time.Second {
			out = append(out, j)
		}
	}
	return out, nil
}

// DeleteAgentCronJob removes a job by ID. Idempotent.
func DeleteAgentCronJob(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM agent_cron_jobs WHERE id = ?`, id)
	return err
}

// UpdateAgentCronJobLastRun stamps the most recent fire time and
// status. status is a short tag (e.g. "ok", "no_target", "send_failed")
// the dashboard surfaces as a pill.
func UpdateAgentCronJobLastRun(id int64, when time.Time, status string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agent_cron_jobs SET last_run_at = ?, last_run_status = ?
		WHERE id = ?`, when.UTC().Format(time.RFC3339), status, id)
	return err
}

// SetAgentCronJobEnabled flips the enabled flag without touching
// the last_run_at timestamp (so re-enabling a paused job doesn't
// immediately fire if it ran recently).
func SetAgentCronJobEnabled(id int64, enabled bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agent_cron_jobs SET enabled = ? WHERE id = ?`,
		boolToInt(enabled), id)
	return err
}

// AgentCronRun is a row in agent_cron_runs — one entry per
// scheduler-fire of a cron job. Lets `cron logs` show the recent
// execution history without mining slog output.
type AgentCronRun struct {
	ID       int64
	JobID    int64
	FiredAt  time.Time
	Status   string
	ErrorMsg string
}

// InsertAgentCronRun appends one execution record. Returns the
// row ID; in practice callers ignore it.
func InsertAgentCronRun(r *AgentCronRun) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT INTO agent_cron_runs
		(job_id, fired_at, status, error_msg)
		VALUES (?, ?, ?, ?)`,
		r.JobID, r.FiredAt.UTC().Format(time.RFC3339), r.Status, r.ErrorMsg)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAgentCronRunsForJob returns the most-recent runs for one job,
// newest first. limit caps the result set; pass 0 for "no limit".
func ListAgentCronRunsForJob(jobID int64, limit int) ([]*AgentCronRun, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT id, job_id, fired_at, status, error_msg
		FROM agent_cron_runs WHERE job_id = ? ORDER BY fired_at DESC`
	args := []any{jobID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentCronRun
	for rows.Next() {
		var r AgentCronRun
		var fired string
		if err := rows.Scan(&r.ID, &r.JobID, &fired, &r.Status, &r.ErrorMsg); err != nil {
			return nil, err
		}
		r.FiredAt = parseTimeOrZero(fired)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func scanAgentCronJob(s rowScanner) (*AgentCronJob, error) {
	var j AgentCronJob
	var enabled int
	var created, lastRun string
	err := s.Scan(&j.ID, &j.Name, &j.OwnerConv, &j.TargetConv, &j.GroupID,
		&j.IntervalSeconds, &j.Subject, &j.Body, &enabled, &created,
		&lastRun, &j.LastRunStatus)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	j.CreatedAt = parseTimeOrZero(created)
	j.LastRunAt = parseTimeOrZero(lastRun)
	return &j, nil
}
