package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Cron job target kinds — the value stored in agent_cron_jobs.target_kind
// (schema v41+). A job either targets a single conversation or fans out
// to every member of a group.
const (
	// CronTargetConv — target_conv is the recipient; group_id (when >0)
	// is the routing group a conv-targeted message is sent through, or 0
	// for a direct tmux send-keys. The long-standing job shape.
	CronTargetConv = "conv"
	// CronTargetGroup — group_id IS the target group; the scheduler
	// resolves that group's membership at fire time and delivers the
	// body to every current member. target_conv is unused.
	CronTargetGroup = "group"
)

// AgentCronJob is a row in agent_cron_jobs. Recurring scheduled
// task that the agentd scheduler fires on a wall-clock interval.
type AgentCronJob struct {
	ID              int64
	Name            string
	OwnerConv       string
	TargetKind      string // CronTargetConv | CronTargetGroup
	TargetConv      string // recipient when TargetKind == CronTargetConv
	GroupID         int64  // conv-kind: routing group (0 → solo send-keys). group-kind: the target group.
	IntervalSeconds int64
	Subject         string
	Body            string
	Enabled         bool
	CreatedAt       time.Time
	LastRunAt       time.Time // zero value → "never run, due immediately"
	LastRunStatus   string
}

// IsGroupTarget reports whether the job fans out to a group rather than
// delivering to a single conv. Callers MUST use this (not GroupID > 0)
// as the discriminator — a conv-targeted job routed through a shared
// group also carries a non-zero GroupID.
func (j *AgentCronJob) IsGroupTarget() bool {
	return j.TargetKind == CronTargetGroup
}

// InsertAgentCronJob writes a new job row. Returns the new ID.
// CreatedAt is stamped server-side; the caller's value is ignored.
func InsertAgentCronJob(j *AgentCronJob) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	kind := j.TargetKind
	if kind == "" {
		kind = CronTargetConv
	}
	res, err := d.Exec(`INSERT INTO agent_cron_jobs
		(name, owner_conv, target_kind, target_conv, group_id, interval_seconds,
		 subject, body, enabled, created_at, last_run_at, last_run_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '')`,
		j.Name, j.OwnerConv, kind, j.TargetConv, j.GroupID, j.IntervalSeconds,
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
	row := d.QueryRow(`SELECT id, name, owner_conv, target_kind, target_conv, group_id,
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
	rows, err := d.Query(`SELECT id, name, owner_conv, target_kind, target_conv, group_id,
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

// UpdateCronPatch is the partial-update shape for UpdateAgentCronJobFields.
// nil → leave field unchanged. Pointer-shaped so callers can distinguish
// "set to zero" from "don't touch".
type UpdateCronPatch struct {
	Name            *string
	OwnerConv       *string
	TargetKind      *string
	TargetConv      *string
	GroupID         *int64
	IntervalSeconds *int64
	Subject         *string
	Body            *string
	Enabled         *bool
}

// UpdateAgentCronJobFields applies a partial update to one row. Only
// non-nil fields in the patch are written. Returns the number of rows
// affected (0 → no such id).
//
// Never touches last_run_at or last_run_status — re-enabling a paused
// job after a long pause must not fire a flood of catch-ups, and
// editing the body should not reset the run-history pointer either.
func UpdateAgentCronJobFields(id int64, p UpdateCronPatch) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	sets := make([]string, 0, 9)
	args := make([]any, 0, 10)
	if p.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *p.Name)
	}
	if p.OwnerConv != nil {
		sets = append(sets, "owner_conv = ?")
		args = append(args, *p.OwnerConv)
	}
	if p.TargetKind != nil {
		sets = append(sets, "target_kind = ?")
		args = append(args, *p.TargetKind)
	}
	if p.TargetConv != nil {
		sets = append(sets, "target_conv = ?")
		args = append(args, *p.TargetConv)
	}
	if p.GroupID != nil {
		sets = append(sets, "group_id = ?")
		args = append(args, *p.GroupID)
	}
	if p.IntervalSeconds != nil {
		sets = append(sets, "interval_seconds = ?")
		args = append(args, *p.IntervalSeconds)
	}
	if p.Subject != nil {
		sets = append(sets, "subject = ?")
		args = append(args, *p.Subject)
	}
	if p.Body != nil {
		sets = append(sets, "body = ?")
		args = append(args, *p.Body)
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*p.Enabled))
	}
	if len(sets) == 0 {
		return 0, nil
	}
	args = append(args, id)
	res, err := d.Exec(`UPDATE agent_cron_jobs SET `+strings.Join(sets, ", ")+
		` WHERE id = ?`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
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
	err := s.Scan(&j.ID, &j.Name, &j.OwnerConv, &j.TargetKind, &j.TargetConv, &j.GroupID,
		&j.IntervalSeconds, &j.Subject, &j.Body, &enabled, &created,
		&lastRun, &j.LastRunStatus)
	if err != nil {
		return nil, err
	}
	if j.TargetKind == "" {
		j.TargetKind = CronTargetConv
	}
	j.Enabled = enabled != 0
	j.CreatedAt = parseTimeOrZero(created)
	j.LastRunAt = parseTimeOrZero(lastRun)
	return &j, nil
}
