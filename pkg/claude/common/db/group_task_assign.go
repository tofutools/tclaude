package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task board assignment and reports (tcl-2eeakl). A task has at most one
// assignee. An assignment never retires, releases or otherwise touches the
// agent: a done report closes the task and leaves the agent where it is.

// Event kinds written by the assign/report slice.
const (
	GroupTaskEventAssigned   = "assigned"
	GroupTaskEventUnassigned = "unassigned"
	GroupTaskEventTaken      = "taken"
	GroupTaskEventReleased   = "released"
	GroupTaskEventReported   = "reported"
)

const (
	GroupTaskOutcomeSucceeded = "succeeded"
	GroupTaskOutcomeFailed    = "failed"

	// GroupTaskFailureLimit is how many failed reports block a task.
	GroupTaskFailureLimit = 3
	// GroupTaskOutcomeNoteMax bounds a report's note, in bytes.
	GroupTaskOutcomeNoteMax = 4 << 10
)

// ErrGroupTaskNotAssignee is a report by an agent that is not the task's
// current assignee.
var ErrGroupTaskNotAssignee = errors.New("you are not the current assignee of this task")

// AssignGroupTask makes agentID the task's assignee. take records it as the
// agent taking the task for itself (event taken) rather than being assigned
// (event assigned). An idea is promoted to the end of the queue on an assign
// and on a take alike; an agent takes only an unassigned task. A task already assigned to someone else is
// reassigned by an assign, refused by a take.
//
// expectAssignee is the holder the caller was authorised against ("" for
// unassigned, AnyAssignee for no check): a task that changed hands meanwhile
// is refused.
func AssignGroupTask(id, agentID, assignedBy string, take bool, actor, expectAssignee string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if expectAssignee != AnyAssignee && t.AssigneeID != expectAssignee && t.AssigneeID != agentID {
			return errAssigneeChanged(id)
		}
		if t.Status != GroupTaskOpen {
			return taskConflict("not_open", "task %s is %s; only an open task can be assigned or taken", id, t.Status)
		}
		if t.AssigneeID == agentID {
			return nil
		}
		if take {
			if t.AssigneeID != "" {
				return taskConflict("assigned", "task %s is assigned to %s; ask whoever assigned it", id, t.AssigneeID)
			}
		}
		// An idea is promoted to the end of the queue on the way, whether it
		// is assigned or taken.
		if t.Stage == GroupTaskStageIdea {
			_, rank, err := placeTask(tx, t.GroupID, GroupTaskStageNext, id, GroupTaskPosition{Bottom: true})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE group_tasks SET stage = 'next', rank = ? WHERE id = ?`, rank, id); err != nil {
				return err
			}
			if err := addGroupTaskChangeTx(tx, id, actor, GroupTaskEventPromoted, "on assign", stageChange(t.Stage, GroupTaskStageNext), now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET assignee_id = ?, assigned_by = ?, assigned_at = ? WHERE id = ?`,
			agentID, assignedBy, dbTime(now), id); err != nil {
			return err
		}
		kind, detail := GroupTaskEventAssigned, "→ "+agentID
		if take {
			kind, detail = GroupTaskEventTaken, agentID
		}
		if t.AssigneeID != "" {
			detail += " (was " + t.AssigneeID + ")"
		}
		return addGroupTaskChangeTx(tx, id, actor, kind, detail, assigneeChange(t.AssigneeID, agentID), now)
	})
}

// UnassignGroupTask clears the task's assignee. release records it as the
// assignee giving the task back (event released). Unassigned already is a
// conflict.
func UnassignGroupTask(id string, release bool, detail, actor, expectAssignee string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.AssigneeID == "" {
			return taskConflict("not_assigned", "task %s is not assigned", id)
		}
		if expectAssignee != AnyAssignee && t.AssigneeID != expectAssignee {
			return errAssigneeChanged(id)
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET assignee_id = NULL, assigned_by = '', assigned_at = NULL WHERE id = ?`,
			id); err != nil {
			return err
		}
		kind := GroupTaskEventUnassigned
		if release {
			kind = GroupTaskEventReleased
		}
		return addGroupTaskChangeTx(tx, id, actor, kind, firstNonEmptyString(detail, t.AssigneeID), assigneeChange(t.AssigneeID, ""), now)
	})
}

// GroupTaskReport is the outcome of ReportGroupTask.
type GroupTaskReport struct {
	Task *GroupTask
	// Repeated is set when the same report by the same agent was already
	// applied; nothing changed.
	Repeated bool
	// Blocked is set when this failed report blocked the task.
	Blocked bool
}

// ReportGroupTask applies the assignee's report. The caller must be the
// task's current assignee; a repeat of the last applied report by the same
// agent, with no assignment since, is acknowledged without being applied
// again. done closes the task (the assignee stays recorded, the agent is left
// alone); failed counts a failure, unassigns, and blocks the task at the
// limit.
func ReportGroupTask(id, agentID, outcome, note, actor string) (*GroupTaskReport, error) {
	if outcome != GroupTaskOutcomeSucceeded && outcome != GroupTaskOutcomeFailed {
		return nil, taskConflict("invalid_arg", "outcome is %s or %s", GroupTaskOutcomeSucceeded, GroupTaskOutcomeFailed)
	}
	detail := truncateUTF8(outcome+": "+note, GroupTaskEventDetailMax)
	out := &GroupTaskReport{}
	task, err := withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		// A repeat is the same report: same actor, same outcome and the same
		// full note as the last one applied, with no assignment since.
		sameAsLast := t.Outcome == outcome && t.OutcomeNote == note
		if t.AssigneeID != agentID {
			repeat, err := isRepeatedGroupTaskReport(tx, id, actor, detail)
			repeat = repeat && sameAsLast
			if err != nil {
				return err
			}
			if repeat {
				out.Repeated = true
				return nil
			}
			return ErrGroupTaskNotAssignee
		}
		if t.Status == GroupTaskDone || t.Status == GroupTaskDropped {
			repeat, err := isRepeatedGroupTaskReport(tx, id, actor, detail)
			repeat = repeat && sameAsLast
			if err != nil {
				return err
			}
			if repeat {
				out.Repeated = true
				return nil
			}
			return taskConflict("not_live", "task %s is already %s", id, t.Status)
		}
		if err := addGroupTaskEventTx(tx, id, actor, GroupTaskEventReported, detail, now); err != nil {
			return err
		}
		// The report's note is a typed comment.
		if err := addGroupTaskCommentTx(tx, id, actor, GroupTaskCommentReportNote, note, now); err != nil {
			return err
		}
		if outcome == GroupTaskOutcomeSucceeded {
			if _, err := tx.Exec(`UPDATE group_tasks SET status = 'done', block_reason = '', closed_at = ?,
				archived_at = NULL, outcome = ?, outcome_note = ? WHERE id = ?`,
				dbTime(now), outcome, note, id); err != nil {
				return err
			}
			return addGroupTaskChangeTx(tx, id, actor, GroupTaskEventDone, "reported by assignee", statusChange(t.Status, GroupTaskDone), now)
		}
		failures := t.Failures + 1
		if _, err := tx.Exec(`UPDATE group_tasks SET outcome = ?, outcome_note = ?, failures = ?,
			assignee_id = NULL, assigned_by = '', assigned_at = NULL WHERE id = ?`,
			outcome, note, failures, id); err != nil {
			return err
		}
		if err := addGroupTaskChangeTx(tx, id, actor, GroupTaskEventUnassigned, "after a failed report", assigneeChange(t.AssigneeID, ""), now); err != nil {
			return err
		}
		if failures >= GroupTaskFailureLimit && t.Status != GroupTaskBlocked {
			reason := fmt.Sprintf("%d failed attempts", failures)
			if _, err := tx.Exec(`UPDATE group_tasks SET status = 'blocked', block_reason = ? WHERE id = ?`, reason, id); err != nil {
				return err
			}
			out.Blocked = true
			if err := addGroupTaskChangeTx(tx, id, actor, GroupTaskEventBlocked, reason,
				statusChange(t.Status, GroupTaskBlocked), now); err != nil {
				return err
			}
			return addGroupTaskCommentTx(tx, id, actor, GroupTaskCommentBlock, reason, now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Task = task
	return out, nil
}

// isRepeatedGroupTaskReport reports whether the task's last report-or-
// assignment event is this same report by the same actor.
func isRepeatedGroupTaskReport(q taskTx, id, actor, detail string) (bool, error) {
	var kind, lastActor, lastDetail string
	err := q.QueryRow(`SELECT kind, actor, detail FROM group_task_events
		WHERE task_id = ? AND kind IN ('reported','assigned','taken','reopened')
		ORDER BY id DESC LIMIT 1`, id).Scan(&kind, &lastActor, &lastDetail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return kind == GroupTaskEventReported && lastActor == actor && lastDetail == detail, nil
}

// AssignedGroupTask is a task assigned to an agent, with its group's name.
type AssignedGroupTask struct {
	Task      *GroupTask
	GroupName string
}

// ListGroupTasksAssignedTo lists the live tasks assigned to agentID, in every
// active group, oldest assignment first.
func ListGroupTasksAssignedTo(agentID string) ([]AssignedGroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT g.name, t.id FROM group_tasks t JOIN agent_groups g ON g.id = t.group_id
		WHERE t.assignee_id = ? AND t.status IN ('open','blocked') AND g.archived_at IS NULL
		ORDER BY t.assigned_at, t.id`, agentID)
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

// TaskboardLock is a group's writer lock.
type TaskboardLock struct {
	Locked    bool
	Writers   []string
	SetBy     string
	UpdatedAt time.Time
}

// GetTaskboardLock reads a group's writer lock; no row is unlocked.
func GetTaskboardLock(groupID int64) (TaskboardLock, error) {
	d, err := Open()
	if err != nil {
		return TaskboardLock{}, err
	}
	var writers, by string
	var at dbTimestamp
	err = d.QueryRow(`SELECT writers, set_by, updated_at FROM group_taskboard_locks WHERE group_id = ?`, groupID).
		Scan(&writers, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskboardLock{}, nil
	}
	if err != nil {
		return TaskboardLock{}, err
	}
	lock := TaskboardLock{Locked: true, SetBy: by, UpdatedAt: at.Time()}
	if err := json.Unmarshal([]byte(writers), &lock.Writers); err != nil {
		return TaskboardLock{}, fmt.Errorf("group %d taskboard writers: %w", groupID, err)
	}
	return lock, nil
}

// SetTaskboardLock locks a group's board to writers (agent ids; an empty
// list leaves only owners, grant holders and the operator), or unlocks it
// when writers is nil.
func SetTaskboardLock(groupID int64, writers []string, by string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	if writers == nil {
		_, err := d.Exec(`DELETE FROM group_taskboard_locks WHERE group_id = ?`, groupID)
		return err
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, w := range writers {
		if w = strings.TrimSpace(w); w != "" && !seen[w] {
			seen[w] = true
			clean = append(clean, w)
		}
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO group_taskboard_locks (group_id, writers, set_by, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET writers = excluded.writers, set_by = excluded.set_by,
			updated_at = excluded.updated_at`, groupID, string(b), by, dbTime(time.Now()))
	return err
}

// unassignAgentGroupTasksTx unassigns every live task assigned to agentID,
// recording why in each task's history.
func unassignAgentGroupTasksTx(tx taskTx, agentID, actor, detail string, now time.Time) error {
	if _, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail)
		SELECT id, ?, ?, ?, ? FROM group_tasks WHERE assignee_id = ? AND status IN ('open','blocked')`,
		dbTime(now), actor, GroupTaskEventUnassigned, detail, agentID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE group_tasks SET assignee_id = NULL, assigned_by = '', assigned_at = NULL, updated_at = ?
		WHERE assignee_id = ? AND status IN ('open','blocked')`, dbTime(now), agentID)
	return err
}
