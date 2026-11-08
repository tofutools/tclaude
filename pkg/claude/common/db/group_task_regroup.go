package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Moving a task to another group of this node (tcl-0r0obl). A move keeps
// the task and its history; a clone is a CreateGroupTask in the other group
// with an Origin note.

// GroupTaskEventRegrouped records a task moved from one group to another;
// GroupTaskEventCloned, on the source task, a clone made of it in another.
const (
	GroupTaskEventRegrouped = "regrouped"
	GroupTaskEventCloned    = "cloned"
)

// NoteGroupTaskCloned records on task id that a clone of it was made.
func NoteGroupTaskCloned(id, actor, detail string) error {
	_, err := withGroupTask(id, func(tx *sql.Tx, _ *GroupTask, now time.Time) error {
		return addGroupTaskEventTx(tx, id, actor, GroupTaskEventCloned, detail, now)
	})
	return err
}

// GroupTaskRegroup is a move of a task to another group.
type GroupTaskRegroup struct {
	ToGroupID int64
	// FromGroup and ToGroup are the groups' names, for the event.
	FromGroup string
	ToGroup   string
	// Stage is where an open task lands: idea (the default) or next. An
	// assignee that is kept puts it in next, as an assign does.
	Stage string
	// KeepAssignee keeps the assignee: the caller found it a member of the
	// target group. Otherwise the assignee is cleared.
	KeepAssignee bool
	Actor        string
}

// GroupTaskRegroupResult is the moved task and what the move undid.
type GroupTaskRegroupResult struct {
	Task *GroupTask
	// DroppedDeps are the tasks this one depended on; DroppedDependents the
	// tasks that depended on it. A dependency never crosses groups.
	DroppedDeps       []string
	DroppedDependents []string
	// Unassigned is the assignee the move cleared, "" when none was.
	Unassigned string
}

// RegroupGroupTask moves task id to another group. Its history stays, with
// a regrouped event that names the old group. A done task stays done;
// anything else lands open (a block is lifted, a dropped task comes back).
func RegroupGroupTask(id string, in GroupTaskRegroup) (*GroupTaskRegroupResult, error) {
	res := &GroupTaskRegroupResult{}
	out, err := withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.GroupID == in.ToGroupID {
			return taskConflict("same_group", "task %s is already in %s", id, in.ToGroup)
		}
		if !t.ArchivedAt.IsZero() {
			return taskConflict("archived", "task %s is archived", id)
		}
		status := GroupTaskOpen
		if t.Status == GroupTaskDone {
			status = GroupTaskDone
		}
		if status == GroupTaskOpen {
			var live int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM group_tasks WHERE group_id = ? AND status IN ('open','blocked')`,
				in.ToGroupID).Scan(&live); err != nil {
				return err
			}
			if live >= GroupTaskLiveCap {
				return ErrGroupTaskCap
			}
		}
		deps, err := groupTaskDepsTx(tx, id)
		if err != nil {
			return err
		}
		dependents, err := groupTaskDependentsTx(tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM group_task_deps WHERE task_id = ? OR depends_on = ?`, id, id); err != nil {
			return err
		}
		// A task that waited on this one no longer does; it stays blocked.
		if _, err := tx.Exec(`UPDATE group_tasks SET blocked_on = '' WHERE blocked_on = ?`, id); err != nil {
			return err
		}
		res.DroppedDeps, res.DroppedDependents = deps, dependents

		assignee := t.AssigneeID
		if assignee != "" && !in.KeepAssignee {
			res.Unassigned, assignee = assignee, ""
		}
		stage := in.Stage
		if stage == "" {
			stage = GroupTaskStageIdea
		}
		if assignee != "" {
			stage = GroupTaskStageNext
		}
		rank := t.Rank
		if status == GroupTaskOpen {
			if stage, rank, err = placeTask(tx, in.ToGroupID, stage, id, GroupTaskPosition{Bottom: true}); err != nil {
				return err
			}
		}
		closed := any(nil)
		if status == GroupTaskDone {
			closed = dbTime(t.ClosedAt)
		}
		if assignee == "" {
			_, err = tx.Exec(`UPDATE group_tasks SET group_id = ?, stage = ?, status = ?, rank = ?, block_reason = '', blocked_on = '',
				closed_at = ?, assignee_id = NULL, assignee_kind = '', started_at = NULL, assigned_by = '', assigned_at = NULL WHERE id = ?`,
				in.ToGroupID, stage, status, rank, closed, id)
		} else {
			_, err = tx.Exec(`UPDATE group_tasks SET group_id = ?, stage = ?, status = ?, rank = ?, block_reason = '', blocked_on = '',
				closed_at = ? WHERE id = ?`, in.ToGroupID, stage, status, rank, closed, id)
		}
		if err != nil {
			return err
		}
		detail := fmt.Sprintf("from %s to %s", in.FromGroup, in.ToGroup)
		if status == GroupTaskOpen {
			detail += ", in " + stage
		}
		if t.Status != status {
			detail += ", was " + t.Status
		}
		if dropped := append(append([]string{}, deps...), dependents...); len(dropped) > 0 {
			detail += ", dependencies dropped: " + strings.Join(dropped, " ")
		}
		if res.Unassigned != "" {
			detail += ", unassigned " + res.Unassigned + " (not in " + in.ToGroup + ")"
		}
		return addGroupTaskChangeTx(tx, id, in.Actor, GroupTaskEventRegrouped, detail,
			[]GroupTaskChange{{Field: "group", Before: in.FromGroup, After: in.ToGroup}}, now)
	})
	if err != nil {
		return nil, err
	}
	res.Task = out
	return res, nil
}

// groupTaskDependentsTx lists the tasks that depend on id.
func groupTaskDependentsTx(q taskTx, id string) ([]string, error) {
	rows, err := q.Query(`SELECT task_id FROM group_task_deps WHERE depends_on = ? ORDER BY task_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}
