package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Epics (tk_a970ef38): a task of kind epic is a container of tasks of its
// group. It has no assignee and no work stage of its own: it sits in the
// ideas while an idea and is open once promoted, and its progress is derived
// from its children. One level: an epic has no parent and is no child.
//
// When the last open child of an open epic closes, the epic is marked
// close-suggested (the board shows a "close epic?" nudge and its creator is
// mailed once); nothing closes it by itself.

const (
	// GroupTaskKindEpic is the kind of an epic.
	GroupTaskKindEpic = "epic"

	// GroupTaskEventParent is a task joining, leaving or changing its epic,
	// on the task; GroupTaskEventChildAdded and GroupTaskEventChildRemoved
	// are the same change on the epic(s).
	GroupTaskEventParent       = "parent"
	GroupTaskEventChildAdded   = "child_added"
	GroupTaskEventChildRemoved = "child_removed"
	// GroupTaskEventCloseSuggested marks an epic whose children are all
	// closed.
	GroupTaskEventCloseSuggested = "close_suggested"
)

// ErrGroupTaskEpic refuses what an epic does not do (assign, take, start,
// report, block, a column).
func errGroupTaskEpic(id, what string) error {
	return taskConflict("epic", "task %s is an epic; an epic is not %s, its children are", id, what)
}

// checkNewGroupTaskKind checks a create's kind against its start state: an
// epic starts open, with no assignee, column, question, blocked task or
// parent.
func checkNewGroupTaskKind(in NewGroupTask, status string) error {
	switch in.Kind {
	case "":
		return nil
	case GroupTaskKindEpic:
	default:
		return taskConflict("invalid_arg", "a task's kind is epic or none, not %s", in.Kind)
	}
	switch {
	case status != GroupTaskOpen:
		return taskConflict("invalid_arg", "an epic starts open")
	case in.AssigneeID != "" || in.Hold != "":
		return taskConflict("invalid_arg", "an epic has no assignee; its children do")
	case in.Column != "":
		return taskConflict("invalid_arg", "an epic sits in no column")
	case in.Question != nil:
		return taskConflict("invalid_arg", "an epic is not a question")
	case in.Blocks != "":
		return taskConflict("invalid_arg", "an epic blocks no task; add the task to it instead")
	case in.ParentID != "":
		return taskConflict("epic", "an epic has no parent")
	}
	return nil
}

// checkGroupTaskParentTx checks that parentID can be the epic of a task of
// groupID (self, when set, is the task itself): an epic of the same group,
// not the task, and the task is no epic.
func checkGroupTaskParentTx(q taskTx, groupID int64, self *GroupTask, parentID string) (*GroupTask, error) {
	if self != nil && self.IsEpic() {
		return nil, taskConflict("epic", "task %s is an epic; an epic has no parent", self.ID)
	}
	if self != nil && self.ID == parentID {
		return nil, taskConflict("invalid_arg", "a task is not its own epic")
	}
	p, err := getGroupTaskTx(q, parentID)
	if errors.Is(err, ErrGroupTaskNotFound) {
		return nil, taskConflict("invalid_arg", "no task %s", parentID)
	}
	if err != nil {
		return nil, err
	}
	if p.GroupID != groupID {
		return nil, taskConflict("invalid_arg", "task %s is on another group's board; an epic holds tasks of its own group", parentID)
	}
	if !p.IsEpic() {
		return nil, taskConflict("not_epic", "task %s is not an epic; make the parent with add --epic", parentID)
	}
	return p, nil
}

// setGroupTaskParentTx moves t into parentID ("" for none), recording the
// change on the task and on the epics it leaves and joins.
func setGroupTaskParentTx(q taskTx, t *GroupTask, parentID, actor string, now time.Time) error {
	if t.ParentID == parentID {
		return nil
	}
	if parentID != "" {
		if _, err := checkGroupTaskParentTx(q, t.GroupID, t, parentID); err != nil {
			return err
		}
	}
	var parent any
	if parentID != "" {
		parent = parentID
	}
	if _, err := q.Exec(`UPDATE group_tasks SET parent_id = ? WHERE id = ?`, parent, t.ID); err != nil {
		return err
	}
	detail := "to epic " + parentID
	switch {
	case parentID == "":
		detail = "out of epic " + t.ParentID
	case t.ParentID != "":
		detail = "from epic " + t.ParentID + " to epic " + parentID
	}
	if err := addGroupTaskChangeTx(q, t.ID, actor, GroupTaskEventParent, detail,
		[]GroupTaskChange{{Field: "parent_id", Before: t.ParentID, After: parentID}}, now); err != nil {
		return err
	}
	if t.ParentID != "" {
		if err := addGroupTaskEventTx(q, t.ParentID, actor, GroupTaskEventChildRemoved, t.ID+" "+truncateUTF8(t.Title, 200), now); err != nil {
			return err
		}
	}
	if parentID != "" {
		if err := addGroupTaskEventTx(q, parentID, actor, GroupTaskEventChildAdded, t.ID+" "+truncateUTF8(t.Title, 200), now); err != nil {
			return err
		}
	}
	return nil
}

// refreshEpicTx brings epic id's close suggestion up to date: set (with an
// event) when it is live, has children and none of them is live; cleared
// otherwise. An archived epic with a child out of the archive comes out of
// it too, so a visible child never points at an archived epic.
func refreshEpicTx(q taskTx, id string, now time.Time) error {
	if id == "" {
		return nil
	}
	var kind, status string
	var suggested sql.NullInt64
	var archived sql.NullInt64
	err := q.QueryRow(`SELECT kind, status, close_suggested_at, archived_at FROM group_tasks WHERE id = ?`, id).
		Scan(&kind, &status, &suggested, &archived)
	if errors.Is(err, sql.ErrNoRows) || err == nil && kind != GroupTaskKindEpic {
		return nil
	}
	if err != nil {
		return err
	}
	var children, live, unarchived int
	if err := q.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status IN ('open','blocked')), 0), COALESCE(SUM(archived_at IS NULL), 0)
		FROM group_tasks WHERE parent_id = ?`, id).Scan(&children, &live, &unarchived); err != nil {
		return err
	}
	if archived.Valid && unarchived > 0 {
		if _, err := q.Exec(`UPDATE group_tasks SET archived_at = NULL WHERE id = ?`, id); err != nil {
			return err
		}
		if err := addGroupTaskEventTx(q, id, "system", GroupTaskEventUnarchived, "a child is out of the archive", now); err != nil {
			return err
		}
	}
	eligible := (status == GroupTaskOpen || status == GroupTaskBlocked) && children > 0 && live == 0
	switch {
	case eligible && !suggested.Valid:
		if _, err := q.Exec(`UPDATE group_tasks SET close_suggested_at = ?, close_nudged = 0 WHERE id = ?`, dbTime(now), id); err != nil {
			return err
		}
		return addGroupTaskEventTx(q, id, "system", GroupTaskEventCloseSuggested, "all children are closed", now)
	case !eligible && suggested.Valid:
		_, err := q.Exec(`UPDATE group_tasks SET close_suggested_at = NULL, close_nudged = 0 WHERE id = ?`, id)
		return err
	}
	return nil
}

// ClaimGroupTaskCloseNudge claims the one mail about epic id's close
// suggestion: true once per suggestion, false when there is none or it was
// claimed already.
func ClaimGroupTaskCloseNudge(id string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE group_tasks SET close_nudged = 1
		WHERE id = ? AND kind = 'epic' AND close_suggested_at IS NOT NULL AND close_nudged = 0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DismissGroupTaskCloseSuggestion hides an epic's "close epic?" nudge ("not
// yet") until a child changes again.
func DismissGroupTaskCloseSuggestion(id, actor string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if !t.IsEpic() {
			return taskConflict("not_epic", "task %s is not an epic", id)
		}
		if t.CloseSuggestedAt.IsZero() {
			return nil
		}
		// Dismissed: keep close_nudged set so the dismissal is not mailed
		// about, and leave close_suggested_at clear until a child changes.
		_, err := tx.Exec(`UPDATE group_tasks SET close_suggested_at = NULL, close_nudged = 1 WHERE id = ?`, id)
		return err
	})
}

// GroupTaskEpicProgress counts an epic's children by where they stand. A
// dropped child is not part of the epic's work: Total leaves it out.
type GroupTaskEpicProgress struct {
	Total      int `json:"total"`
	Done       int `json:"done"`
	Dropped    int `json:"dropped"`
	Blocked    int `json:"blocked"`
	InProgress int `json:"in_progress"`
	// InColumn counts the open children standing in a custom column (In
	// review, QA, …); Columns breaks that down by column slug.
	InColumn int            `json:"in_column"`
	Columns  map[string]int `json:"columns,omitempty"`
	Next     int            `json:"next"`
	Ideas    int            `json:"ideas"`
}

// GroupTaskEpicProgresses returns the progress of each of the given epics
// that has children; one indexed read by parent_id.
func GroupTaskEpicProgresses(ids []string) (map[string]*GroupTaskEpicProgress, error) {
	out := map[string]*GroupTaskEpicProgress{}
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(ids); i += 500 {
		chunk := ids[i:min(i+500, len(ids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		rows, err := d.Query(`SELECT parent_id, status, stage, column_slug, started_at IS NOT NULL, COUNT(*)
			FROM group_tasks WHERE parent_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)
			GROUP BY parent_id, status, stage, column_slug, started_at IS NOT NULL`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var parent, status, stage, column string
			var started bool
			var n int
			if err := rows.Scan(&parent, &status, &stage, &column, &started, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			p := out[parent]
			if p == nil {
				p = &GroupTaskEpicProgress{}
				out[parent] = p
			}
			if status == GroupTaskDropped {
				p.Dropped += n
				continue
			}
			p.Total += n
			switch {
			case status == GroupTaskDone:
				p.Done += n
			case status == GroupTaskBlocked:
				p.Blocked += n
			case column != "":
				p.InColumn += n
				if p.Columns == nil {
					p.Columns = map[string]int{}
				}
				p.Columns[column] += n
			case started:
				p.InProgress += n
			case stage == GroupTaskStageNext:
				p.Next += n
			default:
				p.Ideas += n
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListGroupTaskChildren lists an epic's children, archived ones included:
// the live ones in board order, then the closed ones, most recently closed
// first.
func ListGroupTaskChildren(epicID string) ([]*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tasks, err := queryRows(d, scanGroupTask, `SELECT `+groupTaskCols+` FROM group_tasks WHERE parent_id = ?
		ORDER BY CASE WHEN status IN ('open','blocked') THEN 0 ELSE 1 END,
			CASE WHEN status IN ('open','blocked') THEN CASE stage WHEN 'next' THEN 0 ELSE 1 END ELSE 0 END,
			CASE WHEN status IN ('open','blocked') THEN rank END, closed_at DESC, id`, epicID)
	if err != nil {
		return nil, err
	}
	deps, err := groupTaskDepsOf(d, tasks)
	if err != nil {
		return nil, err
	}
	byID := map[string]*GroupTask{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	for _, p := range deps {
		if t := byID[p[0]]; t != nil {
			t.DependsOn = append(t.DependsOn, p[1])
		}
	}
	return tasks, nil
}

// checkEpicStatusTx refuses what an epic's status does not do: it is never
// blocked (its children are), and it is done only once its children are
// closed, unless forced.
func checkEpicStatusTx(q taskTx, t *GroupTask, status string, force bool) error {
	switch status {
	case GroupTaskBlocked:
		return errGroupTaskEpic(t.ID, "blocked")
	case GroupTaskDone:
		if force {
			return nil
		}
		n, err := liveGroupTaskChildrenTx(q, t.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return taskConflict("epic_open_children", "epic %s has %d open %s; close them first, or close it anyway with --force", t.ID, n, plural(n, "child", "children"))
		}
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// epicWaitsForChildrenTx reports whether id is an epic with a child not in
// the archive, which keeps it out of the archive.
func epicWaitsForChildrenTx(q taskTx, id string) (bool, error) {
	var waits bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM group_tasks WHERE parent_id = ? AND archived_at IS NULL)`, id).Scan(&waits)
	return waits, err
}

// archiveEpicChildrenTx archives an epic's closed children with it; a live
// child keeps it out of the archive.
func archiveEpicChildrenTx(q taskTx, epic *GroupTask, actor string, now time.Time) error {
	n, err := liveGroupTaskChildrenTx(q, epic.ID)
	if err != nil {
		return err
	}
	if n > 0 {
		return taskConflict("epic_open_children", "epic %s has %d open %s; an epic is archived with its children", epic.ID, n, plural(n, "child", "children"))
	}
	ids, err := queryRows(q, func(s rowScanner) (string, error) {
		var id string
		return id, s.Scan(&id)
	}, `SELECT id FROM group_tasks WHERE parent_id = ? AND archived_at IS NULL ORDER BY id`, epic.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := q.Exec(`UPDATE group_tasks SET archived_at = ?, updated_at = ? WHERE id = ?`, dbTime(now), dbTime(now), id); err != nil {
			return err
		}
		if err := addGroupTaskEventTx(q, id, actor, GroupTaskEventArchived, "with epic "+epic.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// liveGroupTaskChildrenTx counts an epic's open and blocked children.
func liveGroupTaskChildrenTx(q taskTx, id string) (int, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM group_tasks WHERE parent_id = ? AND status IN ('open','blocked')`, id).Scan(&n)
	return n, err
}

// releaseGroupTaskChildrenTx takes every child out of a dropped epic.
func releaseGroupTaskChildrenTx(q taskTx, epic *GroupTask, actor string, now time.Time) error {
	children, err := queryRows(q, func(s rowScanner) (*GroupTask, error) {
		var c GroupTask
		return &c, s.Scan(&c.ID, &c.GroupID, &c.Title, &c.Kind)
	}, `SELECT id, group_id, title, kind FROM group_tasks WHERE parent_id = ? ORDER BY id`, epic.ID)
	if err != nil {
		return err
	}
	for _, c := range children {
		c.ParentID = epic.ID
		if err := setGroupTaskParentTx(q, c, "", actor, now); err != nil {
			return err
		}
	}
	return nil
}
