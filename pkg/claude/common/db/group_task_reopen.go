package db

import (
	"database/sql"
	"time"
)

// GroupTaskReopen is a reopen of a done or dropped task (tk_1223214b): back
// on the board where it was, or in another stage, and with its assignee kept,
// changed or taken off, in one write.
type GroupTaskReopen struct {
	// Stage is where it lands: next or idea; "" keeps the stage it had. An
	// assigned task lands in next.
	Stage string
	// Assignee is who holds it after: KeepAssignee keeps the one it had
	// (unless that agent has been retired since), "" takes it off, else an
	// agent id or OperatorAssignee.
	Assignee   string
	AssignedBy string
	Actor      string
	// ExpectAssignee is the holder the caller was authorised against.
	ExpectAssignee string
	// Position, when it names a task (Before or After), places the task
	// beside it, in that task's stage, in the same write.
	Position GroupTaskPosition
}

// KeepAssignee is a GroupTaskReopen that keeps the task's assignee.
const KeepAssignee = "\x00keep"

// ReopenGroupTask reopens a done or dropped task. Each part is recorded in
// the task's history: reopened, then assigned or unassigned, then promoted
// or demoted.
func ReopenGroupTask(id string, in GroupTaskReopen) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.Live() {
			return taskConflict("not_closed", "task %s is %s; only a done or dropped task is reopened", id, t.Status)
		}
		if answered, err := questionTaskAnsweredTx(tx, t); err != nil {
			return err
		} else if answered {
			return taskConflict("answered", "task %s is an answered question; ask again with ask-operator", id)
		}
		if in.Stage != "" && in.Stage != GroupTaskStageIdea && in.Stage != GroupTaskStageNext {
			return taskConflict("invalid_arg", "stage is idea or next")
		}
		if err := setGroupTaskStatusTx(tx, t, GroupTaskOpen, "", in.Actor, in.ExpectAssignee, now); err != nil {
			return err
		}
		cur, err := getGroupTaskTx(tx, id)
		if err != nil {
			return err
		}
		assignee := cur.AssigneeID
		if in.Assignee != KeepAssignee {
			assignee = in.Assignee
		}
		stage := cur.Stage
		if in.Stage != "" {
			stage = in.Stage
		}
		if assignee != "" {
			if in.Stage == GroupTaskStageIdea {
				return taskConflict("invalid_arg", "an assigned task is in next, not an idea; take the assignee off to reopen it as an idea")
			}
			stage = GroupTaskStageNext
		}
		switch assignee {
		case cur.AssigneeID:
			// An assigned task comes back started, even one its assignee
			// held when it was closed.
			if assignee != "" && cur.StartedAt.IsZero() {
				if _, err := tx.Exec(`UPDATE group_tasks SET started_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
					return err
				}
			}
		case "":
			if _, err := tx.Exec(`UPDATE group_tasks SET assignee_id = NULL, assignee_kind = '', started_at = NULL, assigned_by = '', assigned_at = NULL WHERE id = ?`, id); err != nil {
				return err
			}
			if err := addGroupTaskChangeTx(tx, id, in.Actor, GroupTaskEventUnassigned, "on reopen", assigneeChange(cur.AssigneeID, ""), now); err != nil {
				return err
			}
		default:
			if err := setGroupTaskAssigneeTx(tx, id, assignee, in.AssignedBy, true, now); err != nil {
				return err
			}
			detail := "→ " + assignee
			if cur.AssigneeID != "" {
				detail += " (was " + cur.AssigneeID + ")"
			}
			if err := addGroupTaskChangeTx(tx, id, in.Actor, GroupTaskEventAssigned, detail, assigneeChange(cur.AssigneeID, assignee), now); err != nil {
				return err
			}
		}
		anchored := in.Position.Before != "" || in.Position.After != ""
		if stage == cur.Stage && !anchored {
			return nil
		}
		pos := GroupTaskPosition{Bottom: true}
		if stage == GroupTaskStageIdea {
			pos = GroupTaskPosition{Top: true}
		}
		if anchored {
			pos = in.Position
		}
		// An anchor names its own stage.
		stage, rank, err := placeTask(tx, cur.GroupID, stage, id, pos)
		if err != nil {
			return err
		}
		if assignee != "" && stage != GroupTaskStageNext {
			return taskConflict("invalid_arg", "an assigned task is in next, not an idea; take the assignee off to reopen it as an idea")
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET stage = ?, rank = ? WHERE id = ?`, stage, rank, id); err != nil {
			return err
		}
		return stageChangeEvent(tx, cur, stage, in.Actor, "on reopen", now)
	})
}
