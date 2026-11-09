package db

import (
	"database/sql"
	"fmt"
	"strings"
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
		if t.IsEpic() {
			// An epic goes into the archive with its children, never ahead
			// of them.
			if err := archiveEpicChildrenTx(tx, t, actor, now); err != nil {
				return err
			}
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
	return ArchiveClosedGroupTasks(groupID, []string{GroupTaskDone}, closedBefore, actor)
}

// ArchiveClosedGroupTasks archives a group's closed tasks in the given
// statuses (done, dropped or both) not yet archived that were closed before
// closedBefore (zero: all of them), and returns their ids, oldest first.
func ArchiveClosedGroupTasks(groupID int64, statuses []string, closedBefore time.Time, actor string) ([]string, error) {
	if len(statuses) == 0 {
		return nil, fmt.Errorf("archive closed tasks: no status given")
	}
	marks := make([]string, 0, len(statuses))
	for _, s := range statuses {
		if s != GroupTaskDone && s != GroupTaskDropped {
			return nil, fmt.Errorf("archive closed tasks: %q is not a closed status", s)
		}
		marks = append(marks, "?")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// The literal status and archived_at terms let idx_group_tasks_history
	// serve this: only the group's closed, not yet archived tasks.
	where := `group_id = ? AND archived_at IS NULL AND status IN ('done','dropped') AND status IN (` + strings.Join(marks, ", ") + `)`
	args := []any{groupID}
	for _, s := range statuses {
		args = append(args, s)
	}
	if !closedBefore.IsZero() {
		where += ` AND closed_at IS NOT NULL AND closed_at < ?`
		args = append(args, dbTime(closedBefore))
	}
	type row struct{ id, status string }
	rows, err := queryRows(tx, func(s rowScanner) (row, error) {
		var r row
		return r, s.Scan(&r.id, &r.status)
	}, `SELECT id, status FROM group_tasks WHERE `+where+` ORDER BY kind = 'epic', closed_at, id`, args...)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		// Epics come last: one whose children are not all archived by now
		// stays out until they are.
		if waits, err := epicWaitsForChildrenTx(tx, r.id); err != nil || waits {
			if err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET archived_at = ?, updated_at = ? WHERE id = ?`, dbTime(now), dbTime(now), r.id); err != nil {
			return nil, err
		}
		if err := addGroupTaskEventTx(tx, r.id, actor, GroupTaskEventArchived, "archive "+r.status, now); err != nil {
			return nil, err
		}
		ids = append(ids, r.id)
	}
	return ids, tx.Commit()
}
