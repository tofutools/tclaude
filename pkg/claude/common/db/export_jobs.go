package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Export job statuses. The lifecycle is:
//
//	cloning → requested → running → ready
//	   ↘  ↘  ↘  ↘  ↘  ↘  ↘  ↘  ↘  ↘   failed   (clone/agent error or timeout)
//
// cloning:   the daemon created the job and is spawning an isolated CLONE of the
// conversation to produce the export on (so the live original is never disturbed
// — JOH-266). The clone's conv-id is recorded in WorkerConvID.
// requested: the clone is alive and its pane has been nudged.
// running:   the agent (the clone) fetched the brief (`tclaude agent export show`)
// — it has the request and is working on it.
// ready:     the agent uploaded the artifact; it is downloadable. The clone is
// then auto-deleted.
// failed:    the clone/agent reported a failure, or the cleanup sweep aged the
// job out before an artifact arrived. The reason is in ExportJob.Error.
const (
	ExportStatusCloning   = "cloning"
	ExportStatusRequested = "requested"
	ExportStatusRunning   = "running"
	ExportStatusReady     = "ready"
	ExportStatusFailed    = "failed"
)

// ErrExportJobNotFound is returned by GetExportJob when no row has the id.
var ErrExportJobNotFound = errors.New("export job not found")

// ExportJob is one per-agent export request — a row of the export_jobs table,
// the store behind the dashboard's "📋 summary…" action (see migrateV66toV67).
//
// ConvID is the ORIGINAL conversation the export is about (the history list and
// the download attach to it). WorkerConvID is the isolated CLONE the daemon
// spawns to actually produce the summary (JOH-266) — it is who gets nudged, who
// submits the artifact, and whose identity the /v1 ownership gate accepts; it is
// blank until the clone is spawned, and the clone is auto-deleted once the job
// is ready. Keeping the two split is what lets the summary attach to the original
// while the live original is never disturbed.
//
// Title / Instructions / Preset are the human's brief, snapshotted at creation.
// The Artifact* fields are blank until the agent uploads its result:
// ArtifactPath is the on-disk file under ~/.tclaude/exports/<id>/, ArtifactName
// the download filename the browser sees, ContentType its MIME type.
type ExportJob struct {
	ID           int64
	ConvID       string
	WorkerConvID string
	GroupName    string
	Title        string
	Instructions string
	Preset       string
	Status       string
	Error        string
	ArtifactPath string
	ArtifactName string
	ArtifactSize int64
	ContentType  string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// InsertExportJob records a new export request and returns its id. CreatedAt /
// UpdatedAt default to now, and Status defaults to ExportStatusRequested, when
// the caller leaves them zero.
func InsertExportJob(j *ExportJob) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now()
	created := j.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := j.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	status := j.Status
	if status == "" {
		status = ExportStatusRequested
	}
	res, err := d.Exec(`
		INSERT INTO export_jobs
			(conv_id, worker_conv_id, group_name, title, instructions, preset, status, error,
			 artifact_path, artifact_name, artifact_size, content_type,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ConvID, j.WorkerConvID, j.GroupName, j.Title, j.Instructions, j.Preset, status, j.Error,
		j.ArtifactPath, j.ArtifactName, j.ArtifactSize, j.ContentType,
		created.Format(time.RFC3339Nano), updated.Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("insert export job: %w", err)
	}
	return res.LastInsertId()
}

// GetExportJob loads one job by id. Returns ErrExportJobNotFound when no row
// matches, so callers can map it to a 404 distinctly from a real DB error.
func GetExportJob(id int64) (*ExportJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT id, conv_id, worker_conv_id, group_name, title, instructions, preset, status, error,
		       artifact_path, artifact_name, artifact_size, content_type,
		       created_at, updated_at
		FROM export_jobs WHERE id = ?`, id)
	j, err := scanExportJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrExportJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// SetExportJobWorkerConv records the conv-id of the isolated clone the daemon
// spawned to produce the export (JOH-266). Written as soon as the clone's
// conv-id is known — before it is nudged — so the cleanup sweep can reap the
// clone even if the daemon dies mid-flight. Leaves the status untouched (the job
// stays 'cloning' until the clone is alive and nudged). updated_at is refreshed
// so the stale timer measures from when the clone was actually minted.
//
// Returns whether a row was updated (false = the job was deleted / cleared
// between insert and this write). The caller MUST treat false as "the worker
// identity was not persisted" and reap the clone rather than nudge it: an
// un-recorded clone is rejected by the /v1 ownership gate and is invisible to
// the cleanup sweep, so continuing would both fail the export and leak the clone.
func SetExportJobWorkerConv(id int64, workerConvID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`UPDATE export_jobs SET worker_conv_id = ?, updated_at = ? WHERE id = ?`,
		workerConvID, time.Now().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, fmt.Errorf("set export job worker conv: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkExportJobRequested flips a job from 'cloning' to 'requested' — the clone
// is alive and its pane has been nudged. Only advances a job still in 'cloning'
// (monotonic: a job already past it is left untouched), so a late call after the
// agent already fetched the brief can't drag the status backwards. Returns
// whether it moved.
func MarkExportJobRequested(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`UPDATE export_jobs SET status = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		ExportStatusRequested, time.Now().Format(time.RFC3339Nano), id, ExportStatusCloning)
	if err != nil {
		return false, fmt.Errorf("mark export job requested: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkExportJobRunning flips a job to 'running' — the agent fetched the brief.
// Only advances a job still in 'requested' (idempotent and monotonic: a job
// already ready/failed/running is left untouched). Returns whether it moved.
func MarkExportJobRunning(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`UPDATE export_jobs SET status = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		ExportStatusRunning, time.Now().Format(time.RFC3339Nano), id, ExportStatusRequested)
	if err != nil {
		return false, fmt.Errorf("mark export job running: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetExportJobReady records a successful upload: the artifact's on-disk path,
// download name, size and MIME type, flipping the job to 'ready' and clearing
// any prior error. Accepts a job in any NON-ready state (a late artifact can
// still revive a timed-out 'failed' job) but refuses to overwrite a job that is
// already 'ready' — a second/duplicate submit must not clobber the delivered
// artifact's metadata. Returns whether a row was updated (false = the job is
// gone or already ready); the caller decides what that means.
func SetExportJobReady(id int64, path, name string, size int64, contentType string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`
		UPDATE export_jobs
		SET status = ?, error = '', artifact_path = ?, artifact_name = ?,
		    artifact_size = ?, content_type = ?, updated_at = ?
		WHERE id = ? AND status != ?`,
		ExportStatusReady, path, name, size, contentType,
		time.Now().Format(time.RFC3339Nano), id, ExportStatusReady)
	if err != nil {
		return false, fmt.Errorf("set export job ready: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FailExportJob flips a job to 'failed' with a reason, unless it is already
// 'ready' (a delivered artifact is authoritative — a late timeout sweep must
// not clobber a job the agent successfully completed). Returns whether it moved.
func FailExportJob(id int64, reason string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`UPDATE export_jobs SET status = ?, error = ?, updated_at = ?
		 WHERE id = ? AND status != ?`,
		ExportStatusFailed, reason, time.Now().Format(time.RFC3339Nano), id, ExportStatusReady)
	if err != nil {
		return false, fmt.Errorf("fail export job: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListExportJobsForConv returns a conversation's export jobs, newest first
// (by id = insertion order, NOT created_at — the RFC3339Nano lexical-sort
// hazard, see ListHumanMessages). limit <= 0 returns all of them. Powers the
// modal's "Previous exports" history panel.
func ListExportJobsForConv(convID string, limit int) ([]*ExportJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `
		SELECT id, conv_id, worker_conv_id, group_name, title, instructions, preset, status, error,
		       artifact_path, artifact_name, artifact_size, content_type,
		       created_at, updated_at
		FROM export_jobs WHERE conv_id = ? ORDER BY id DESC`
	args := []any{convID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*ExportJob
	for rows.Next() {
		j, err := scanExportJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteExportJobsForConv hard-deletes every export job for a conversation and
// returns their ids so the caller can remove the on-disk artifact dirs. The
// "clear all" control behind the history panel.
//
// A single `DELETE … RETURNING id` (SQLite 3.35+, supported by the driver)
// deletes and reports the affected ids atomically — so a job that becomes ready
// concurrently can't be deleted-but-missed, which would orphan its artifact dir.
func DeleteExportJobsForConv(convID string) ([]int64, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`DELETE FROM export_jobs WHERE conv_id = ? RETURNING id`, convID)
	if err != nil {
		return nil, fmt.Errorf("delete export jobs for conv: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListStaleExportJobs returns jobs whose updated_at is older than `before` —
// the cleanup sweep's input. `terminalOnly` selects only ready/failed jobs
// (TTL prune of finished work + its artifacts); when false it returns every
// stale job (used to time out requested/running jobs that never completed).
//
// Parsed and filtered in Go rather than via a lexical SQL comparison on the
// RFC3339Nano string, which would misorder around whole-second boundaries.
func ListStaleExportJobs(before time.Time, terminalOnly bool) ([]*ExportJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT id, conv_id, worker_conv_id, group_name, title, instructions, preset, status, error,
		       artifact_path, artifact_name, artifact_size, content_type,
		       created_at, updated_at
		FROM export_jobs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*ExportJob
	for rows.Next() {
		j, err := scanExportJob(rows)
		if err != nil {
			return nil, err
		}
		if j.UpdatedAt.IsZero() || !j.UpdatedAt.Before(before) {
			continue
		}
		terminal := j.Status == ExportStatusReady || j.Status == ExportStatusFailed
		if terminalOnly && !terminal {
			continue
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeleteExportJob hard-deletes a job by id. The on-disk artifact directory is
// the caller's responsibility (the cleanup sweep removes it before this). A
// non-existent id is a no-op, not an error. Returns whether a row was removed.
func DeleteExportJob(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM export_jobs WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete export job: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// scanExportJob reads one export_jobs row, parsing the RFC3339Nano timestamps.
// Takes the package-shared rowScanner (see agent.go) so both *sql.Row and
// *sql.Rows callers reuse it.
// A corrupt timestamp leaves the field zero (logged) rather than failing the
// whole read — the same tolerance ListHumanMessages applies.
func scanExportJob(s rowScanner) (*ExportJob, error) {
	var j ExportJob
	var created, updated string
	if err := s.Scan(&j.ID, &j.ConvID, &j.WorkerConvID, &j.GroupName, &j.Title, &j.Instructions,
		&j.Preset, &j.Status, &j.Error, &j.ArtifactPath, &j.ArtifactName,
		&j.ArtifactSize, &j.ContentType, &created, &updated); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		j.CreatedAt = t
	} else {
		slog.Warn("export_jobs: unparseable created_at, leaving zero",
			"id", j.ID, "value", created, "error", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, updated); err == nil {
		j.UpdatedAt = t
	} else {
		slog.Warn("export_jobs: unparseable updated_at, leaving zero",
			"id", j.ID, "value", updated, "error", err)
	}
	return &j, nil
}
