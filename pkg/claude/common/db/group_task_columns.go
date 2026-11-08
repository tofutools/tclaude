package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Custom stage columns (tcl-plby5h): a group may name a few columns of its
// own, such as "In review" or "In CI". They sit between In progress and
// Blocked, in the group's order. A live task of stage next stands in at most
// one of them (group_tasks.column_slug); it keeps its assignee, and closing
// it, demoting it, or removing the column takes it out again (the triggers of
// migrateV324toV325). The built-in columns are not stored here.

// GroupTaskColumnsCap is how many custom columns a group holds.
const GroupTaskColumnsCap = 4

// GroupTaskEventColumn records a task entering or leaving a custom column;
// its change is the "column" field, before and after (slugs).
const GroupTaskEventColumn = "column"

// GroupTaskColumn is one custom column of a group's board.
type GroupTaskColumn struct {
	GroupID   int64
	Slug      string
	Label     string
	Position  int
	CreatedBy string
	CreatedAt time.Time
}

// ErrGroupTaskColumnNotFound is a slug that names no column of the group.
var ErrGroupTaskColumnNotFound = errors.New("no such column")

func listGroupTaskColumnsTx(q taskTx, groupID int64) ([]GroupTaskColumn, error) {
	return queryRows(q, func(s rowScanner) (GroupTaskColumn, error) {
		var c GroupTaskColumn
		var created dbTimestamp
		err := s.Scan(&c.GroupID, &c.Slug, &c.Label, &c.Position, &c.CreatedBy, &created)
		c.CreatedAt = created.Time()
		return c, err
	}, `SELECT group_id, slug, label, position, created_by, created_at FROM group_task_columns
		WHERE group_id = ? ORDER BY position, slug`, groupID)
}

// ListGroupTaskColumns lists a group's custom columns in their order.
func ListGroupTaskColumns(groupID int64) ([]GroupTaskColumn, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listGroupTaskColumnsTx(d, groupID)
}

// withGroupTaskColumns runs fn on a group's columns in a transaction, then
// writes their order back as fn left it (positions 0..n-1).
func withGroupTaskColumns(groupID int64, fn func(tx *sql.Tx, cols []GroupTaskColumn, now time.Time) ([]GroupTaskColumn, error)) ([]GroupTaskColumn, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	cols, err := listGroupTaskColumnsTx(tx, groupID)
	if err != nil {
		return nil, err
	}
	cols, err = fn(tx, cols, time.Now())
	if err != nil {
		return nil, err
	}
	for i, c := range cols {
		if _, err := tx.Exec(`UPDATE group_task_columns SET position = ?, label = ? WHERE group_id = ? AND slug = ?`,
			i, c.Label, groupID, c.Slug); err != nil {
			return nil, err
		}
	}
	out, err := listGroupTaskColumnsTx(tx, groupID)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func columnIndex(cols []GroupTaskColumn, slug string) int {
	for i, c := range cols {
		if c.Slug == slug {
			return i
		}
	}
	return -1
}

// insertColumnAt places c at index i of cols (clamped to the ends).
func insertColumnAt(cols []GroupTaskColumn, c GroupTaskColumn, i int) []GroupTaskColumn {
	if i < 0 || i > len(cols) {
		i = len(cols)
	}
	out := append([]GroupTaskColumn{}, cols[:i]...)
	out = append(out, c)
	return append(out, cols[i:]...)
}

// AddGroupTaskColumn adds a custom column to a group's board, before the
// column named by before ("" for the end). The slug must be new to the group.
func AddGroupTaskColumn(groupID int64, slug, label, before, actor string) ([]GroupTaskColumn, error) {
	return withGroupTaskColumns(groupID, func(tx *sql.Tx, cols []GroupTaskColumn, now time.Time) ([]GroupTaskColumn, error) {
		if columnIndex(cols, slug) >= 0 {
			return nil, taskConflict("column_exists", "the board already has a column %s", slug)
		}
		for _, c := range cols {
			if strings.EqualFold(c.Label, label) {
				return nil, taskConflict("column_exists", "the board already has a column named %q", label)
			}
		}
		if len(cols) >= GroupTaskColumnsCap {
			return nil, taskConflict("cap", "a board holds at most %d custom columns", GroupTaskColumnsCap)
		}
		at := len(cols)
		if before != "" {
			if at = columnIndex(cols, before); at < 0 {
				return nil, ErrGroupTaskColumnNotFound
			}
		}
		if _, err := tx.Exec(`INSERT INTO group_task_columns (group_id, slug, label, position, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, groupID, slug, label, len(cols), actor, dbTime(now)); err != nil {
			return nil, err
		}
		return insertColumnAt(cols, GroupTaskColumn{GroupID: groupID, Slug: slug, Label: label}, at), nil
	})
}

// GroupTaskColumnChange renames a column (Label) and/or moves it before
// another column (Before) or to the end (ToEnd).
type GroupTaskColumnChange struct {
	Label  string
	Before string
	ToEnd  bool
}

// ChangeGroupTaskColumn renames and/or reorders a group's custom column.
func ChangeGroupTaskColumn(groupID int64, slug string, ch GroupTaskColumnChange) ([]GroupTaskColumn, error) {
	return withGroupTaskColumns(groupID, func(_ *sql.Tx, cols []GroupTaskColumn, _ time.Time) ([]GroupTaskColumn, error) {
		i := columnIndex(cols, slug)
		if i < 0 {
			return nil, ErrGroupTaskColumnNotFound
		}
		c := cols[i]
		if ch.Label != "" && ch.Label != c.Label {
			for _, o := range cols {
				if o.Slug != slug && strings.EqualFold(o.Label, ch.Label) {
					return nil, taskConflict("column_exists", "the board already has a column named %q", ch.Label)
				}
			}
			c.Label = ch.Label
		}
		if ch.Before == slug {
			return nil, taskConflict("invalid_arg", "a column cannot move before itself")
		}
		if ch.Before == "" && !ch.ToEnd {
			cols[i] = c
			return cols, nil
		}
		rest := append(append([]GroupTaskColumn{}, cols[:i]...), cols[i+1:]...)
		at := len(rest)
		if ch.Before != "" {
			if at = columnIndex(rest, ch.Before); at < 0 {
				return nil, ErrGroupTaskColumnNotFound
			}
		}
		return insertColumnAt(rest, c, at), nil
	})
}

// RemoveGroupTaskColumn removes a group's custom column. The tasks that
// stood in it go back to next (In progress for a started one), each with a
// column change in its history; their ids are returned.
func RemoveGroupTaskColumn(groupID int64, slug, actor string) ([]string, error) {
	var moved []string
	_, err := withGroupTaskColumns(groupID, func(tx *sql.Tx, cols []GroupTaskColumn, now time.Time) ([]GroupTaskColumn, error) {
		i := columnIndex(cols, slug)
		if i < 0 {
			return nil, ErrGroupTaskColumnNotFound
		}
		ids, err := queryRows(tx, func(s rowScanner) (string, error) {
			var id string
			return id, s.Scan(&id)
		}, `SELECT id FROM group_tasks WHERE group_id = ? AND column_slug = ? ORDER BY rank, id`, groupID, slug)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if err := addGroupTaskChangeTx(tx, id, actor, GroupTaskEventColumn, "column "+cols[i].Label+" removed",
				[]GroupTaskChange{{Field: "column", Before: slug, After: ""}}, now); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE group_tasks SET updated_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
				return nil, err
			}
		}
		// The delete trigger clears the tasks' column_slug.
		if _, err := tx.Exec(`DELETE FROM group_task_columns WHERE group_id = ? AND slug = ?`, groupID, slug); err != nil {
			return nil, err
		}
		moved = ids
		return append(append([]GroupTaskColumn{}, cols[:i]...), cols[i+1:]...), nil
	})
	return moved, err
}

// SetGroupTaskColumn puts a live task in one of its group's custom columns,
// or takes it out (slug ""). An idea is promoted to the end of the queue
// first: a column stands in next. expectAssignee is the holder the caller
// was authorised against (AnyAssignee for none).
func SetGroupTaskColumn(id, slug, actor, expectAssignee string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if expectAssignee != AnyAssignee && t.AssigneeID != expectAssignee {
			return errAssigneeChanged(id)
		}
		if t.Column == slug {
			return nil
		}
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it first", id, t.Status)
		}
		label := ""
		if slug != "" {
			cols, err := listGroupTaskColumnsTx(tx, t.GroupID)
			if err != nil {
				return err
			}
			i := columnIndex(cols, slug)
			if i < 0 {
				return taskConflict("invalid_arg", "the board has no column %s", slug)
			}
			label = cols[i].Label
		}
		if t.Stage != GroupTaskStageNext {
			_, rank, err := placeTask(tx, t.GroupID, GroupTaskStageNext, id, GroupTaskPosition{Bottom: true})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE group_tasks SET stage = 'next', rank = ? WHERE id = ?`, rank, id); err != nil {
				return err
			}
			if err := stageChangeEvent(tx, t, GroupTaskStageNext, actor, "into "+label, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET column_slug = ? WHERE id = ?`, slug, id); err != nil {
			return err
		}
		detail := "→ " + label
		if slug == "" {
			detail = "out of " + t.Column
		}
		return addGroupTaskChangeTx(tx, id, actor, GroupTaskEventColumn, detail,
			[]GroupTaskChange{{Field: "column", Before: t.Column, After: slug}}, now)
	})
}
