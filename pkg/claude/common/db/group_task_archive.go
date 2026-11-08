package db

import (
	"database/sql"
	"time"
)

// Archiving a task by hand (tk_2f3e574f, tk_4f591970): an archived task
// leaves every list and the history (as the hourly sweep's archive does), but
// is kept, and comes back with UnarchiveGroupTask. It is purged with the
// other closed tasks after taskboard.retention_days.

// GroupTaskEventUnarchived is a task taken out of the archive.
const GroupTaskEventUnarchived = "unarchived"

// ArchiveGroupTask archives a done or dropped task. Archiving an archived
// task is a no-op.
func ArchiveGroupTask(id, actor string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.Live() {
			return taskConflict("not_closed", "task %s is %s; close or drop it first", id, t.Status)
		}
		if !t.ArchivedAt.IsZero() {
			return nil
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET archived_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
			return err
		}
		return addGroupTaskEventTx(tx, id, actor, GroupTaskEventArchived, "by hand", now)
	})
}

// UnarchiveGroupTask takes an archived task out of the archive, back into
// the history (done or dropped, as it was, with its closing time): one closed
// longer ago than taskboard.archive_after_days is archived again by the next
// sweep. Reopening puts an archived task straight back on the board.
func UnarchiveGroupTask(id, actor string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.ArchivedAt.IsZero() {
			return taskConflict("not_archived", "task %s is not archived", id)
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET archived_at = NULL WHERE id = ?`, id); err != nil {
			return err
		}
		return addGroupTaskEventTx(tx, id, actor, GroupTaskEventUnarchived, "", now)
	})
}

// ArchiveDoneGroupTasks archives a group's done tasks not yet archived that
// were closed before closedBefore (zero: all of them), and returns their
// ids.
func ArchiveDoneGroupTasks(groupID int64, closedBefore time.Time, actor string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	where := `group_id = ? AND status = 'done' AND archived_at IS NULL`
	args := []any{groupID}
	if !closedBefore.IsZero() {
		where += ` AND closed_at IS NOT NULL AND closed_at < ?`
		args = append(args, dbTime(closedBefore))
	}
	ids, err := queryRows(tx, func(s rowScanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT id FROM group_tasks WHERE `+where+` ORDER BY closed_at, id`, args...)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE group_tasks SET archived_at = ?, updated_at = ? WHERE id = ?`, dbTime(now), dbTime(now), id); err != nil {
			return nil, err
		}
		if err := addGroupTaskEventTx(tx, id, actor, GroupTaskEventArchived, "archive done", now); err != nil {
			return nil, err
		}
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, tx.Commit()
}
