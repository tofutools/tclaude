package db

import (
	"database/sql"
	"errors"
	"time"
)

// Idle without report (docs/taskboard.md). A task is stalled when it is open
// and assigned and its assignee has sat idle past taskboard.idle_grace. The
// state is derived from the assignee's session, never stored; the store keeps
// only the one "stalled" event per assignment that the sweep writes when it
// tells the assigner. Stalled never fails, blocks or unassigns a task.

// GroupTaskEventStalled is the event the idle sweep writes, once per
// assignment.
const GroupTaskEventStalled = "stalled"

// ListOpenAssignedGroupTasks is every open, assigned task in an active group:
// the idle sweep's candidates.
func ListOpenAssignedGroupTasks() ([]AssignedGroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT g.name, t.id FROM group_tasks t JOIN agent_groups g ON g.id = t.group_id
		WHERE t.assignee_id IS NOT NULL AND t.status = 'open' AND g.archived_at IS NULL
		ORDER BY t.assigned_at, t.id`)
	if err != nil {
		return nil, err
	}
	type pair struct{ group, id string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.group, &p.id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		pairs = append(pairs, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AssignedGroupTask, 0, len(pairs))
	for _, p := range pairs {
		t, err := getGroupTaskTx(d, p.id)
		if errors.Is(err, ErrGroupTaskNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, AssignedGroupTask{Task: t, GroupName: p.group})
	}
	return out, nil
}

// MarkGroupTaskStalled writes the task's "stalled" event when it is still
// open and assigned to assignee as of assignedAt and no stalled event has been
// written since that assignment began. It reports whether it wrote one: the
// sweep mails only then, so a stall is announced once per assignment. The
// event is history, not an edit: updated_at is left alone.
func MarkGroupTaskStalled(id, assignee string, assignedAt time.Time, detail string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := getGroupTaskTx(tx, id)
	if err != nil {
		return false, err
	}
	if t.Status != GroupTaskOpen || t.AssigneeID != assignee || !t.AssignedAt.Equal(assignedAt) {
		return false, nil
	}
	var kind string
	err = tx.QueryRow(`SELECT kind FROM group_task_events
		WHERE task_id = ? AND kind IN ('assigned','taken','stalled')
		ORDER BY id DESC LIMIT 1`, id).Scan(&kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if kind == GroupTaskEventStalled {
		return false, nil
	}
	if err := addGroupTaskEventTx(tx, id, "system:idle", GroupTaskEventStalled, detail, time.Now()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CountBlockedGroupTasks is the number of blocked tasks per group id.
func CountBlockedGroupTasks() (map[int64]int, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT group_id, COUNT(*) FROM group_tasks WHERE status = 'blocked' GROUP BY group_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int{}
	for rows.Next() {
		var gid int64
		var n int
		if err := rows.Scan(&gid, &n); err != nil {
			return nil, err
		}
		out[gid] = n
	}
	return out, rows.Err()
}
