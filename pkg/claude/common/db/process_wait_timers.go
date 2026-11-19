package db

import (
	"database/sql"
	"errors"
	"time"
)

// ProcessWaitTimer is the deadline of one wait step that is waiting: the
// step's command, when it started, and when it is due. Failure, when set,
// says the deadline could not be worked out; the runtime fails the step with
// it. The row goes away when the wait ends.
type ProcessWaitTimer struct {
	RunID     string
	CommandID string
	NodeID    string
	Attempt   int
	StartedAt time.Time
	DueAt     time.Time
	Failure   string
}

// ProcessUntimedWait is an outstanding wait command that has no timer yet:
// its run planned it, and the daemon has still to fix its deadline.
type ProcessUntimedWait struct {
	RunID     string
	CommandID string
	NodeID    string
	Attempt   int
	Duration  string
	Until     string
	// Now is a wait due at once: a check wait's first check.
	Now bool
}

// MaxProcessWaitPage bounds one page of wait timers.
const MaxProcessWaitPage = 256

const processWaitTimerColumns = `run_id, command_id, node_id, attempt, started_at, due_at, failure`

// EnsureProcessWaitTimer records a wait's timer unless it already has one,
// and returns the one that holds. A deadline is fixed once: a second call,
// after a restart say, never moves it.
func EnsureProcessWaitTimer(timer ProcessWaitTimer) (ProcessWaitTimer, error) {
	d, err := Open()
	if err != nil {
		return ProcessWaitTimer{}, err
	}
	if len(timer.Failure) > 1024 {
		timer.Failure = timer.Failure[:1024]
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO process_wait_timers (`+processWaitTimerColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, timer.RunID, timer.CommandID, timer.NodeID, timer.Attempt,
		dbTime(timer.StartedAt.UTC()), dbTime(timer.DueAt.UTC()), timer.Failure); err != nil {
		return ProcessWaitTimer{}, err
	}
	held, ok, err := GetProcessWaitTimer(timer.RunID, timer.CommandID)
	if err != nil {
		return ProcessWaitTimer{}, err
	}
	if !ok {
		return ProcessWaitTimer{}, errors.New("process wait timer vanished as it was recorded")
	}
	return held, nil
}

// GetProcessWaitTimer is one wait's timer, if it has one.
func GetProcessWaitTimer(runID, commandID string) (ProcessWaitTimer, bool, error) {
	timers, err := queryProcessWaitTimers(`WHERE run_id = ? AND command_id = ?`, runID, commandID)
	if err != nil || len(timers) == 0 {
		return ProcessWaitTimer{}, false, err
	}
	return timers[0], true, nil
}

// ListProcessWaitTimersForRun is every timer a run's waits hold.
func ListProcessWaitTimersForRun(runID string) ([]ProcessWaitTimer, error) {
	return queryProcessWaitTimers(`WHERE run_id = ? ORDER BY node_id LIMIT ?`, runID, MaxProcessWaitPage)
}

// processRunDoomedSQL holds for a run's checkpoint that has been given up
// (engine.Draining): canceled, ended early, or a step failed or canceled.
const processRunDoomedSQL = `(json_valid(checkpoint_json) = 1 AND (
	COALESCE(json_extract(checkpoint_json, '$.canceled'), 0) = 1
	OR COALESCE(json_extract(checkpoint_json, '$.endedEarly'), '') != ''
	OR (json_type(checkpoint_json, '$.nodes') = 'object' AND EXISTS (
		SELECT 1 FROM json_each(checkpoint_json, '$.nodes') WHERE value IN ('failed', 'canceled')))))`

// processWaitTakerSQL selects the runs whose due waits end: running, bound to
// their scope, not paused, and not given up (whose waits are withdrawn).
const processWaitTakerSQL = `SELECT id FROM process_runs WHERE status = 'running' AND scope_state = 'bound'
	AND paused_at IS NULL AND NOT ` + processRunDoomedSQL

// ListDueProcessWaitTimers is the waits due at now whose run can take them:
// running, bound to its scope, and not paused. A paused run's due wait ends
// once the run is resumed.
func ListDueProcessWaitTimers(now time.Time) ([]ProcessWaitTimer, error) {
	return queryProcessWaitTimers(`WHERE due_at <= ? AND run_id IN (`+processWaitTakerSQL+`)
		ORDER BY due_at, run_id, command_id LIMIT ?`, dbTimeBoundary(now.UTC()), MaxProcessWaitPage)
}

// ListDrainingProcessWaitTimers is the waits of running runs that have been
// given up, which are withdrawn rather than waited out.
func ListDrainingProcessWaitTimers() ([]ProcessWaitTimer, error) {
	return queryProcessWaitTimers(`WHERE run_id IN (SELECT id FROM process_runs WHERE status = 'running' AND `+
		processRunDoomedSQL+`) ORDER BY run_id, command_id LIMIT ?`, MaxProcessWaitPage)
}

// NextProcessWaitDue is the earliest deadline among the waits a run can take,
// so the runtime can wake for it.
func NextProcessWaitDue() (time.Time, bool, error) {
	d, err := Open()
	if err != nil {
		return time.Time{}, false, err
	}
	var due dbTimestamp
	err = d.QueryRow(`SELECT MIN(due_at) FROM process_wait_timers WHERE run_id IN (` + processWaitTakerSQL + `)`).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return due.Time(), !due.Time().IsZero(), nil
}

// BringProcessWaitTimerDue makes a wait's timer due at `at` if it is due
// later; it reports whether there was such a timer.
func BringProcessWaitTimerDue(runID, commandID string, at time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	at = at.UTC()
	result, err := d.Exec(`UPDATE process_wait_timers SET due_at = ? WHERE run_id = ? AND command_id = ? AND due_at > ? AND failure = ''`,
		dbTime(at), runID, commandID, dbTimeBoundary(at))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// DeleteProcessWaitTimer drops a wait's timer once the wait has ended.
func DeleteProcessWaitTimer(runID, commandID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM process_wait_timers WHERE run_id = ? AND command_id = ?`, runID, commandID)
	return err
}

// DeleteFinishedProcessWaitTimers drops the timers of runs that are over, whose
// waits were withdrawn with them.
func DeleteFinishedProcessWaitTimers() error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM process_wait_timers WHERE run_id IN (
		SELECT id FROM process_runs WHERE status != 'running')`)
	return err
}

// ListProcessUntimedWaits is the outstanding wait commands of running runs
// that have no timer yet: planned, but the daemon stopped before fixing their
// deadline, or has not got to it.
func ListProcessUntimedWaits() ([]ProcessUntimedWait, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT r.id,
			json_extract(c.value, '$.id'), json_extract(c.value, '$.nodeId'), json_extract(c.value, '$.attempt'),
			COALESCE(json_extract(c.value, '$.wait.duration'), ''), COALESCE(json_extract(c.value, '$.wait.until'), ''),
			COALESCE(json_extract(c.value, '$.wait.now'), 0)
		FROM process_runs AS r, json_each(r.checkpoint_json, '$.commands') AS c
		WHERE r.status = 'running' AND json_valid(r.checkpoint_json) = 1
			AND json_type(r.checkpoint_json, '$.commands') = 'array'
			AND json_extract(c.value, '$.kind') = 'wait'
			AND NOT EXISTS (SELECT 1 FROM process_wait_timers AS t
				WHERE t.run_id = r.id AND t.command_id = json_extract(c.value, '$.id'))
		ORDER BY r.id LIMIT ?`, MaxProcessWaitPage)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var waits []ProcessUntimedWait
	for rows.Next() {
		var wait ProcessUntimedWait
		var commandID, nodeID sql.NullString
		var attempt sql.NullInt64
		if err := rows.Scan(&wait.RunID, &commandID, &nodeID, &attempt, &wait.Duration, &wait.Until, &wait.Now); err != nil {
			return nil, err
		}
		if !commandID.Valid || !nodeID.Valid || !attempt.Valid || attempt.Int64 < 1 {
			continue
		}
		wait.CommandID, wait.NodeID, wait.Attempt = commandID.String, nodeID.String, int(attempt.Int64)
		waits = append(waits, wait)
	}
	return waits, rows.Err()
}

func queryProcessWaitTimers(query string, args ...any) ([]ProcessWaitTimer, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+processWaitTimerColumns+` FROM process_wait_timers `+query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var timers []ProcessWaitTimer
	for rows.Next() {
		var timer ProcessWaitTimer
		var started, due dbTimestamp
		if err := rows.Scan(&timer.RunID, &timer.CommandID, &timer.NodeID, &timer.Attempt, &started, &due, &timer.Failure); err != nil {
			return nil, err
		}
		timer.StartedAt, timer.DueAt = started.Time(), due.Time()
		timers = append(timers, timer)
	}
	return timers, rows.Err()
}
