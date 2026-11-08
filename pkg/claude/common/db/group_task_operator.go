package db

import (
	"database/sql"
	"strings"
	"time"
)

// The operator as an assignee (tcl-p4t78f). The operator of this node can
// hold a task like an agent does. assignee_id is an FK to agents, so an
// operator task stores assignee_kind = 'operator' with assignee_id NULL; on
// read, GroupTask.AssigneeID is OperatorAssignee, so every holder check
// (expectAssignee, take, report) treats the operator as one more holder.

// OperatorAssignee is GroupTask.AssigneeID for a task the operator holds,
// and the name the API takes for the operator ("to":"operator").
const OperatorAssignee = "operator"

// GroupTaskWaitReasonPrefix starts the block reason of a task waiting on an
// operator task.
const GroupTaskWaitReasonPrefix = "waiting on operator: "

// setGroupTaskAssigneeTx stores who holds the task: an agent id, or
// OperatorAssignee.
func setGroupTaskAssigneeTx(tx taskTx, id, assignee, assignedBy string, now time.Time) error {
	agentID, kind := any(assignee), ""
	if assignee == OperatorAssignee {
		agentID, kind = nil, "operator"
	}
	_, err := tx.Exec(`UPDATE group_tasks SET assignee_id = ?, assignee_kind = ?, assigned_by = ?, assigned_at = ? WHERE id = ?`,
		agentID, kind, assignedBy, dbTime(now), id)
	return err
}

// BlockGroupTaskOn makes task id wait on task on: on becomes one of its
// dependencies and it is blocked with the reason "waiting on operator:
// <title>", remembering on, so that closing on as done reopens it. on must
// be a live task of the same group that the operator holds. expectAssignee is the holder of id the
// caller was authorised against.
func BlockGroupTaskOn(id, on, actor, expectAssignee string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		target, err := getGroupTaskTx(tx, on)
		if err != nil {
			return err
		}
		return blockGroupTaskOnTx(tx, t, target, actor, expectAssignee, now)
	})
}

func blockGroupTaskOnTx(tx taskTx, t, on *GroupTask, actor, expectAssignee string, now time.Time) error {
	if expectAssignee != AnyAssignee && t.AssigneeID != expectAssignee {
		return errAssigneeChanged(t.ID)
	}
	if on.GroupID != t.GroupID {
		return taskConflict("invalid_arg", "task %s is on another board; a task waits only on its own board's tasks", on.ID)
	}
	if t.ID == on.ID {
		return taskConflict("invalid_arg", "a task cannot wait on itself")
	}
	if !t.Live() {
		return taskConflict("not_live", "task %s is %s; reopen it first", t.ID, t.Status)
	}
	if on.AssigneeID != OperatorAssignee {
		return taskConflict("invalid_arg", "task %s is not assigned to the operator; block on it with a reason instead", on.ID)
	}
	if !on.Live() {
		return taskConflict("not_live", "task %s is %s; a task waits only on a live task", on.ID, on.Status)
	}
	before := strings.Join(t.DependsOn, " ")
	if err := setGroupTaskDepsTx(tx, t, []string{on.ID}, nil); err != nil {
		return err
	}
	deps, err := groupTaskDepsTx(tx, t.ID)
	if err != nil {
		return err
	}
	if after := strings.Join(deps, " "); after != before {
		if err := addGroupTaskChangeTx(tx, t.ID, actor, GroupTaskEventEdited, "+dep "+on.ID,
			[]GroupTaskChange{{Field: "depends_on", Before: before, After: after}}, now); err != nil {
			return err
		}
	}
	reason := truncateUTF8(GroupTaskWaitReasonPrefix+on.Title, GroupTaskEventDetailMax)
	if _, err := tx.Exec(`UPDATE group_tasks SET status = 'blocked', block_reason = ?, blocked_on = ?, updated_at = ? WHERE id = ?`,
		reason, on.ID, dbTime(now), t.ID); err != nil {
		return err
	}
	var changes []GroupTaskChange
	if t.Status != GroupTaskBlocked {
		changes = statusChange(t.Status, GroupTaskBlocked)
	}
	if err := addGroupTaskChangeTx(tx, t.ID, actor, GroupTaskEventBlocked, reason, changes, now); err != nil {
		return err
	}
	return addGroupTaskCommentTx(tx, t.ID, actor, GroupTaskCommentBlock, reason, now)
}

// releaseGroupTaskWaitersTx reopens the tasks blocked waiting on t, which was
// just closed as done. A task blocked for another reason is left alone.
func releaseGroupTaskWaitersTx(tx taskTx, t *GroupTask, actor string, now time.Time) error {
	rows, err := tx.Query(`SELECT id FROM group_tasks WHERE blocked_on = ? AND status = 'blocked'`, t.ID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE group_tasks SET status = 'open', block_reason = '', blocked_on = '', failures = 0,
			updated_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
			return err
		}
		if err := addGroupTaskChangeTx(tx, id, actor, GroupTaskEventUnblocked, t.ID+" is done",
			statusChange(GroupTaskBlocked, GroupTaskOpen), now); err != nil {
			return err
		}
	}
	return nil
}

// GroupTaskDependents lists the tasks that depend on id.
func GroupTaskDependents(id string) ([]*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	ids, err := groupTaskDependentsTx(d, id)
	if err != nil {
		return nil, err
	}
	out := make([]*GroupTask, 0, len(ids))
	for _, dep := range ids {
		t, err := getGroupTaskTx(d, dep)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}
