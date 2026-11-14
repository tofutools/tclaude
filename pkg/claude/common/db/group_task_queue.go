package db

import (
	"database/sql"
	"time"
)

// Work-in-progress queues (docs/taskboard.md "Limits"). A board may cap how
// many tasks a lane holds: In progress (an agent's started tasks in no
// custom column) and each custom column. A task that would go into a full
// lane may wait for it instead, queued: one for In progress is held in next,
// one for a column stays where it is. The board's limits live in its options
// (boardopts); this file stores who waits, and counts the lanes.

// QueueLaneInProgress names the In progress lane in queued_for; a custom
// column's lane is its slug (which never holds an underscore).
const QueueLaneInProgress = "in_progress"

// GroupTaskEventQueued records a task getting in line for a full lane.
const GroupTaskEventQueued = "queued"

// laneWhere is the condition for a live task in lane, after "WHERE ".
func laneWhere(lane string) (string, []any) {
	if lane == QueueLaneInProgress {
		return `status IN ('open','blocked') AND assignee_id IS NOT NULL AND assignee_kind = '' AND started_at IS NOT NULL
			AND column_slug = ''`, nil
	}
	return `status IN ('open','blocked') AND column_slug = ?`, []any{lane}
}

// GroupTaskLaneCount counts the live tasks in lane on group's board; with
// assignee, only that agent's. A blocked task keeps its place in its lane.
func GroupTaskLaneCount(groupID int64, lane, assignee string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	where, args := laneWhere(lane)
	q := `SELECT COUNT(*) FROM group_tasks WHERE group_id = ? AND ` + where
	args = append([]any{groupID}, args...)
	if assignee != "" {
		q += ` AND assignee_id = ? AND assignee_kind = ''`
		args = append(args, assignee)
	}
	var n int
	err = d.QueryRow(q, args...).Scan(&n)
	return n, err
}

// InGroupTaskLane reports whether t sits in lane.
func (t *GroupTask) InGroupTaskLane(lane string) bool {
	if !t.Live() {
		return false
	}
	if lane == QueueLaneInProgress {
		return t.Column == "" && t.Started() && t.AssigneeID != OperatorAssignee
	}
	return t.Column == lane
}

// GroupTaskLane is the lane t sits in: QueueLaneInProgress, its column's
// slug, or "" for none (an idea, Next, a closed task, the operator's).
func (t *GroupTask) GroupTaskLane() string {
	switch {
	case !t.Live():
		return ""
	case t.Column != "":
		return t.Column
	case t.InGroupTaskLane(QueueLaneInProgress):
		return QueueLaneInProgress
	}
	return ""
}

// QueueGroupTask puts task id in line for lane. A task waiting for In
// progress is held (in next) if it was started; one waiting for a column
// stays where it is. A parked task leaves the backlog for next first: a
// task in line is in play. A task already in line for lane keeps its place.
// expectAssignee is the holder the caller was authorised against.
func QueueGroupTask(id, lane, detail, actor, expectAssignee string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if expectAssignee != AnyAssignee && t.AssigneeID != expectAssignee {
			return errAssigneeChanged(id)
		}
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it first", id, t.Status)
		}
		if t.Stage == GroupTaskStageBacklog {
			if err := setGroupTaskStageTx(tx, t, GroupTaskStageNext, "to get in line", actor, now); err != nil {
				return err
			}
		}
		if lane == QueueLaneInProgress {
			if t.AssigneeID == "" || t.AssigneeID == OperatorAssignee {
				return taskConflict("not_assigned", "task %s has no agent to start it for", id)
			}
			if t.Column == "" && t.Started() {
				if err := startGroupTaskTx(tx, t, false, GroupTaskStageNext, actor, now); err != nil {
					return err
				}
			}
		}
		if t.QueuedFor == lane {
			return nil
		}
		// A separate write after the hold: the clear trigger does not watch
		// queued_for, so this one stands.
		if _, err := tx.Exec(`UPDATE group_tasks SET queued_for = ?, queued_at = ? WHERE id = ?`, lane, dbTime(now), id); err != nil {
			return err
		}
		return addGroupTaskEventTx(tx, id, actor, GroupTaskEventQueued, detail, now)
	})
}

// ClearGroupTaskQueue takes task id out of line.
func ClearGroupTaskQueue(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE group_tasks SET queued_for = '', queued_at = NULL WHERE id = ?`, id)
	return err
}

// ListGroupTaskQueue is group's tasks in line for a lane, first in line
// first: by when each got in line.
func ListGroupTaskQueue(groupID int64) ([]*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, scanGroupTask, `SELECT `+groupTaskCols+` FROM group_tasks
		WHERE group_id = ? AND queued_for <> '' AND status IN ('open','blocked') ORDER BY queued_at, rank, id`, groupID)
}

// ListGroupsWithQueues is the groups with any task in line, for the sweep.
func ListGroupsWithQueues() ([]int64, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, func(s rowScanner) (int64, error) {
		var id int64
		err := s.Scan(&id)
		return id, err
	}, `SELECT DISTINCT group_id FROM group_tasks WHERE queued_for <> '' AND status IN ('open','blocked')`)
}
