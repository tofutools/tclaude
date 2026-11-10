package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// A task's activity stream (tcl-jipq70), after awb's: one append-only stream
// of Markdown comments and change entries. A change is recorded in the same
// transaction as the mutation, and a failed or no-op one records nothing.
// A comment is accepted under a client key, unique per task and author, so
// a retry returns the original. Its author may edit it (the earlier body is
// kept as a comment_edited entry) or delete it (a tombstone; its bodies and
// files go). A block, done or drop reason and a report's note are typed
// comments committed with their transition.

const (
	GroupTaskEventComment       = "comment"
	GroupTaskEventCommentEdited = "comment_edited"

	// What a comment is.
	GroupTaskCommentPlain      = "comment"
	GroupTaskCommentBlock      = "block_reason"
	GroupTaskCommentDone       = "done_reason"
	GroupTaskCommentDrop       = "drop_reason"
	GroupTaskCommentReportNote = "report_note"

	// GroupTaskCommentMaxBytes bounds a comment's Markdown.
	GroupTaskCommentMaxBytes = 16 << 10
	// GroupTaskCommentKeyMax bounds a client key.
	GroupTaskCommentKeyMax = 128
)

var (
	// ErrGroupTaskCommentNotFound answers a comment the task does not have.
	ErrGroupTaskCommentNotFound = errors.New("no such comment")
	// ErrGroupTaskNotCommentAuthor refuses an edit or delete by anyone but
	// the comment's author.
	ErrGroupTaskNotCommentAuthor = errors.New("only a comment's author may change it")
)

// GroupTaskChange is one field's change on a change entry.
type GroupTaskChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func encodeGroupTaskChanges(changes []GroupTaskChange) string {
	if len(changes) == 0 {
		return ""
	}
	for i := range changes {
		changes[i].Before = truncateUTF8(changes[i].Before, GroupTaskCommentMaxBytes)
		changes[i].After = truncateUTF8(changes[i].After, GroupTaskCommentMaxBytes)
	}
	raw, _ := json.Marshal(changes)
	return string(raw)
}

const groupTaskEventCols = `id, task_id, at, actor, kind, detail, changes, body, comment_kind, COALESCE(client_key, ''),
	COALESCE(comment_id, 0), edited_at, deleted_at`

func scanGroupTaskEvent(s rowScanner) (*GroupTaskEvent, error) {
	var e GroupTaskEvent
	var at, edited, deleted dbTimestamp
	var changes string
	if err := s.Scan(&e.ID, &e.TaskID, &at, &e.Actor, &e.Kind, &e.Detail, &changes, &e.Body, &e.CommentKind,
		&e.ClientKey, &e.CommentID, &edited, &deleted); err != nil {
		return nil, err
	}
	e.At, e.EditedAt, e.DeletedAt = at.Time(), edited.Time(), deleted.Time()
	if changes != "" {
		_ = json.Unmarshal([]byte(changes), &e.Changes)
	}
	return &e, nil
}

// addGroupTaskCommentTx records a typed comment (a reason or a note) with
// the transition it explains. An empty body records nothing.
func addGroupTaskCommentTx(q taskTx, taskID, actor, kind, body string, at time.Time) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	_, err := q.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind)
		VALUES (?, ?, ?, ?, '', ?, ?)`, taskID, dbTime(at), actor, GroupTaskEventComment,
		truncateUTF8(body, GroupTaskCommentMaxBytes), kind)
	return err
}

func getGroupTaskCommentTx(q taskTx, taskID string, id int64) (*GroupTaskEvent, error) {
	e, err := scanGroupTaskEvent(q.QueryRow(`SELECT `+groupTaskEventCols+` FROM group_task_events
		WHERE task_id = ? AND id = ? AND kind = ?`, taskID, id, GroupTaskEventComment))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGroupTaskCommentNotFound
	}
	return e, err
}

// NewGroupTaskComment is a comment to add.
type NewGroupTaskComment struct {
	TaskID string
	Actor  string
	Body   string
	// Key is the client's key; "" for none (the comment is not idempotent).
	Key string
	// Attachments are the comment's files, their blobs already stored.
	Attachments []GroupTaskAttachment
}

// AddGroupTaskComment adds a comment. A key already used by this author on
// this task returns that comment (created false) when the body is the same,
// and a key_conflict otherwise.
func AddGroupTaskComment(in NewGroupTaskComment) (comment *GroupTaskEvent, created bool, err error) {
	_, err = withGroupTask(in.TaskID, func(tx *sql.Tx, _ *GroupTask, now time.Time) error {
		if in.Key != "" {
			e, err := scanGroupTaskEvent(tx.QueryRow(`SELECT `+groupTaskEventCols+` FROM group_task_events
				WHERE task_id = ? AND actor = ? AND client_key = ?`, in.TaskID, in.Actor, in.Key))
			if err == nil {
				if e.Kind != GroupTaskEventComment || e.Body != in.Body && e.DeletedAt.IsZero() {
					return taskConflict("key_conflict", "key %q was already used for a different comment", in.Key)
				}
				comment = e
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var key any
		if in.Key != "" {
			key = in.Key
		}
		res, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind, client_key)
			VALUES (?, ?, ?, ?, '', ?, ?, ?)`, in.TaskID, dbTime(now), in.Actor, GroupTaskEventComment, in.Body,
			GroupTaskCommentPlain, key)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if err := insertGroupTaskAttachmentsTx(tx, in.TaskID, id, append([]GroupTaskAttachment(nil), in.Attachments...), in.Actor, now); err != nil {
			return err
		}
		created = true
		comment, err = getGroupTaskCommentTx(tx, in.TaskID, id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return comment, created, nil
}

// EditGroupTaskComment replaces a comment's body, by its author only. The
// earlier body is kept as a comment_edited entry; the same body is a no-op.
func EditGroupTaskComment(taskID string, id int64, actor, body string) (*GroupTaskEvent, error) {
	var out *GroupTaskEvent
	_, err := withGroupTask(taskID, func(tx *sql.Tx, _ *GroupTask, now time.Time) error {
		c, err := getGroupTaskCommentTx(tx, taskID, id)
		if err != nil {
			return err
		}
		if c.Actor != actor {
			return ErrGroupTaskNotCommentAuthor
		}
		if !c.DeletedAt.IsZero() {
			return taskConflict("deleted", "comment %d was deleted", id)
		}
		if c.CommentKind == GroupTaskCommentQuestion || c.CommentKind == GroupTaskCommentAnswer {
			return taskConflict("not_editable", "a question or an answer is not edited; withdraw the question and ask again")
		}
		if c.Body != body {
			if _, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_id)
				VALUES (?, ?, ?, ?, '', ?, ?)`, taskID, dbTime(now), actor, GroupTaskEventCommentEdited, c.Body, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE group_task_events SET body = ?, edited_at = ? WHERE id = ?`, body, dbTime(now), id); err != nil {
				return err
			}
		}
		out, err = getGroupTaskCommentTx(tx, taskID, id)
		return err
	})
	return out, err
}

// DeleteGroupTaskComment leaves a tombstone of a comment, by its author
// only: its body, its earlier bodies and its files go.
func DeleteGroupTaskComment(taskID string, id int64, actor string) (*GroupTaskEvent, error) {
	var out *GroupTaskEvent
	_, err := withGroupTask(taskID, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		c, err := getGroupTaskCommentTx(tx, taskID, id)
		if err != nil {
			return err
		}
		if c.Actor != actor {
			return ErrGroupTaskNotCommentAuthor
		}
		switch c.CommentKind {
		case GroupTaskCommentAnswer:
			return taskConflict("not_editable", "an answer stays in the activity")
		case GroupTaskCommentQuestion:
			// Deleting a question withdraws it, while it is open.
			if c.DeletedAt.IsZero() {
				if err := withdrawGroupTaskQuestionTx(tx, t, id, actor, now); err != nil {
					return err
				}
			}
		}
		if c.DeletedAt.IsZero() {
			for _, stmt := range []struct {
				q    string
				args []any
			}{
				{`UPDATE group_task_events SET body = '', deleted_at = ? WHERE id = ?`, []any{dbTime(now), id}},
				{`UPDATE group_task_events SET body = '' WHERE task_id = ? AND comment_id = ?`, []any{taskID, id}},
				{`DELETE FROM group_task_attachments WHERE task_id = ? AND comment_id = ?`, []any{taskID, id}},
			} {
				if _, err := tx.Exec(stmt.q, stmt.args...); err != nil {
					return err
				}
			}
		}
		out, err = getGroupTaskCommentTx(tx, taskID, id)
		return err
	})
	return out, err
}

// GetGroupTaskComment returns one comment of a task.
func GetGroupTaskComment(taskID string, id int64) (*GroupTaskEvent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return getGroupTaskCommentTx(d, taskID, id)
}

// ListGroupTaskComments lists a task's comments, oldest first, tombstones
// included.
func ListGroupTaskComments(taskID string) ([]*GroupTaskEvent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, scanGroupTaskEvent, `SELECT `+groupTaskEventCols+` FROM group_task_events
		WHERE task_id = ? AND kind = ? ORDER BY at, id`, taskID, GroupTaskEventComment)
}

// GroupTaskActivitySummary is what a task view shows of its stream.
type GroupTaskActivitySummary struct {
	// Comments counts the comments not deleted.
	Comments int
	// LastEditedBy and LastEditedAt are the latest edit of its text, ref or
	// dependencies.
	LastEditedBy string
	LastEditedAt time.Time
}

// GroupTaskActivitySummaries maps each of the given tasks to its summary;
// a task with no comments and no edits is absent.
func GroupTaskActivitySummaries(ids []string) (map[string]GroupTaskActivitySummary, error) {
	out := map[string]GroupTaskActivitySummary{}
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := []any{GroupTaskEventComment}
		for _, id := range chunk {
			args = append(args, id)
		}
		in := `(?` + strings.Repeat(",?", len(chunk)-1) + `)`
		rows, err := d.Query(`SELECT task_id, COUNT(*) FROM group_task_events
			WHERE kind = ? AND deleted_at IS NULL AND task_id IN `+in+` GROUP BY task_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			s := out[id]
			s.Comments = n
			out[id] = s
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		args[0] = GroupTaskEventEdited
		rows, err = d.Query(`SELECT e.task_id, e.actor, e.at FROM group_task_events e
			JOIN (SELECT task_id, MAX(id) AS id FROM group_task_events WHERE kind = ? AND task_id IN `+in+` GROUP BY task_id) last
			ON e.id = last.id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, actor string
			var at dbTimestamp
			if err := rows.Scan(&id, &actor, &at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			s := out[id]
			s.LastEditedBy, s.LastEditedAt = actor, at.Time()
			out[id] = s
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func stageChange(before, after string) []GroupTaskChange {
	return []GroupTaskChange{{Field: "stage", Before: before, After: after}}
}

func statusChange(before, after string) []GroupTaskChange {
	return []GroupTaskChange{{Field: "status", Before: before, After: after}}
}

func assigneeChange(before, after string) []GroupTaskChange {
	return []GroupTaskChange{{Field: "assignee", Before: before, After: after}}
}
