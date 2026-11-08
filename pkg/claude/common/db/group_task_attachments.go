package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Files attached to a task on a group's task board (tcl-jy91ej). A row
// names a content-addressed blob by its sha256; the daemon owns the bytes
// (agentd/taskboard_attachments.go) and collects a blob no row names.

const (
	// GroupTaskAttachmentsCap bounds the files on one task.
	GroupTaskAttachmentsCap = 10

	GroupTaskEventAttached = "attached"
	GroupTaskEventDetached = "detached"
)

// ErrGroupTaskAttachmentNotFound answers a file the task does not have.
var ErrGroupTaskAttachmentNotFound = errors.New("no such attachment")

// GroupTaskAttachment is one file on a task.
type GroupTaskAttachment struct {
	ID          int64
	TaskID      string
	Name        string
	SHA256      string
	SizeBytes   int64
	ContentType string
	AddedBy     string
	AddedAt     time.Time
	// CommentID is the comment the file came with; 0 for the task's own.
	CommentID int64
	// Drawing is a drawing-board export's reference back to its board, as
	// JSON (agentd's taskboardDrawingRef); "" for any other file.
	Drawing string
}

const groupTaskAttachmentCols = `id, task_id, name, sha256, size_bytes, content_type, added_by, added_at, COALESCE(comment_id, 0), COALESCE(drawing, '')`

func scanGroupTaskAttachment(s rowScanner) (GroupTaskAttachment, error) {
	var a GroupTaskAttachment
	var at dbTimestamp
	err := s.Scan(&a.ID, &a.TaskID, &a.Name, &a.SHA256, &a.SizeBytes, &a.ContentType, &a.AddedBy, &at, &a.CommentID, &a.Drawing)
	a.AddedAt = at.Time()
	return a, err
}

// uniqueAttachmentName returns name, or name with an "N-" prefix, so that it
// is unique among taken (lower-cased names; macOS file systems and the
// index fold case). It adds the result to taken.
func uniqueAttachmentName(name string, taken map[string]bool) string {
	out := name
	for i := 2; taken[strings.ToLower(out)]; i++ {
		out = fmt.Sprintf("%d-%s", i, name)
	}
	taken[strings.ToLower(out)] = true
	return out
}

// insertGroupTaskAttachmentsTx adds files to task id inside tx, under the
// cap, renaming a duplicate name, and records an attached event for each.
// commentID, when not 0, makes them that comment's files: the cap is the
// comment's, and the comment itself is their record. It fills in each
// row's ID, TaskID, final Name and AddedAt.
func insertGroupTaskAttachmentsTx(tx taskTx, id string, commentID int64, atts []GroupTaskAttachment, actor string, now time.Time) error {
	if len(atts) == 0 {
		return nil
	}
	// Names are unique across the task, its comments' files included.
	all, err := queryRows(tx, scanGroupTaskAttachment, `SELECT `+groupTaskAttachmentCols+`
		FROM group_task_attachments WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return err
	}
	held := 0
	for _, a := range all {
		if a.CommentID == commentID {
			held++
		}
	}
	if held+len(atts) > GroupTaskAttachmentsCap {
		if commentID != 0 {
			return taskConflict("attachment_cap", "a comment holds at most %d files", GroupTaskAttachmentsCap)
		}
		return taskConflict("attachment_cap", "a task holds at most %d files; %s has %d", GroupTaskAttachmentsCap, id, held)
	}
	taken := map[string]bool{}
	for _, a := range all {
		taken[strings.ToLower(a.Name)] = true
	}
	var comment any
	if commentID != 0 {
		comment = commentID
	}
	for i := range atts {
		a := &atts[i]
		a.TaskID, a.AddedAt = id, now
		a.Name = uniqueAttachmentName(a.Name, taken)
		if a.AddedBy == "" {
			a.AddedBy = actor
		}
		var drawing any
		if a.Drawing != "" {
			drawing = a.Drawing
		}
		res, err := tx.Exec(`INSERT INTO group_task_attachments (task_id, name, sha256, size_bytes, content_type, added_by, added_at, comment_id, drawing)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, a.Name, a.SHA256, a.SizeBytes, a.ContentType, a.AddedBy, dbTime(now), comment, drawing)
		if err != nil {
			return err
		}
		a.ID, _ = res.LastInsertId()
		a.CommentID = commentID
		if commentID != 0 {
			continue
		}
		if err := addGroupTaskEventTx(tx, id, actor, GroupTaskEventAttached, a.Name, now); err != nil {
			return err
		}
	}
	return nil
}

// AddGroupTaskAttachments attaches files to task id and returns the task
// and the rows as stored (a duplicate name gets an "N-" prefix).
func AddGroupTaskAttachments(id string, atts []GroupTaskAttachment, actor string) (*GroupTask, []GroupTaskAttachment, error) {
	rows := append([]GroupTaskAttachment(nil), atts...)
	t, err := withGroupTask(id, func(tx *sql.Tx, _ *GroupTask, now time.Time) error {
		return insertGroupTaskAttachmentsTx(tx, id, 0, rows, actor, now)
	})
	if err != nil {
		return nil, nil, err
	}
	return t, rows, nil
}

// ReplaceGroupTaskAttachmentFile swaps the bytes of task id's file
// attachmentID for repl's (SHA256, SizeBytes, ContentType, Drawing), keeping
// its id and name: a drawing-board export brought up to date. The task's own
// file gets an attached entry saying so; a comment's file is the comment's.
// The old blob stays until the collector finds nothing names it.
func ReplaceGroupTaskAttachmentFile(id string, attachmentID int64, repl GroupTaskAttachment, actor string) (*GroupTask, *GroupTaskAttachment, error) {
	var out GroupTaskAttachment
	t, err := withGroupTask(id, func(tx *sql.Tx, _ *GroupTask, now time.Time) error {
		a, err := scanGroupTaskAttachment(tx.QueryRow(`SELECT `+groupTaskAttachmentCols+`
			FROM group_task_attachments WHERE task_id = ? AND id = ?`, id, attachmentID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGroupTaskAttachmentNotFound
		}
		if err != nil {
			return err
		}
		var drawing any
		if repl.Drawing != "" {
			drawing = repl.Drawing
		}
		if _, err := tx.Exec(`UPDATE group_task_attachments SET sha256 = ?, size_bytes = ?, content_type = ?, drawing = ?, added_by = ?, added_at = ?
			WHERE id = ?`, repl.SHA256, repl.SizeBytes, repl.ContentType, drawing, actor, dbTime(now), attachmentID); err != nil {
			return err
		}
		a.SHA256, a.SizeBytes, a.ContentType, a.Drawing, a.AddedBy, a.AddedAt = repl.SHA256, repl.SizeBytes, repl.ContentType, repl.Drawing, actor, now
		out = a
		if a.CommentID != 0 {
			return nil
		}
		return addGroupTaskEventTx(tx, id, actor, GroupTaskEventAttached, a.Name+" (updated)", now)
	})
	if err != nil {
		return nil, nil, err
	}
	return t, &out, nil
}

// RemoveGroupTaskAttachment detaches one file from task id. The blob stays
// until the collector finds nothing names it.
func RemoveGroupTaskAttachment(id string, attachmentID int64, actor string) (*GroupTask, *GroupTaskAttachment, error) {
	var removed GroupTaskAttachment
	t, err := withGroupTask(id, func(tx *sql.Tx, _ *GroupTask, now time.Time) error {
		// A comment's file goes with its comment, not on its own.
		a, err := scanGroupTaskAttachment(tx.QueryRow(`SELECT `+groupTaskAttachmentCols+`
			FROM group_task_attachments WHERE task_id = ? AND id = ? AND comment_id IS NULL`, id, attachmentID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGroupTaskAttachmentNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM group_task_attachments WHERE id = ?`, attachmentID); err != nil {
			return err
		}
		removed = a
		return addGroupTaskEventTx(tx, id, actor, GroupTaskEventDetached, a.Name, now)
	})
	if err != nil {
		return nil, nil, err
	}
	return t, &removed, nil
}

func listGroupTaskAttachmentsTx(q taskTx, id string) ([]GroupTaskAttachment, error) {
	return queryRows(q, scanGroupTaskAttachment, `SELECT `+groupTaskAttachmentCols+`
		FROM group_task_attachments WHERE task_id = ? AND comment_id IS NULL ORDER BY id`, id)
}

// ListGroupTaskCommentAttachments maps each comment of task id that has
// files to its files.
func ListGroupTaskCommentAttachments(id string) (map[int64][]GroupTaskAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := queryRows(d, scanGroupTaskAttachment, `SELECT `+groupTaskAttachmentCols+`
		FROM group_task_attachments WHERE task_id = ? AND comment_id IS NOT NULL ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	out := map[int64][]GroupTaskAttachment{}
	for _, a := range rows {
		out[a.CommentID] = append(out[a.CommentID], a)
	}
	return out, nil
}

// ListGroupTaskAttachments lists task id's own files (not its comments')
// in the order they were added.
func ListGroupTaskAttachments(id string) ([]GroupTaskAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listGroupTaskAttachmentsTx(d, id)
}

// ListGroupTaskAttachmentsBatch maps each of the given task ids that has
// files to its files.
func ListGroupTaskAttachmentsBatch(ids []string) (map[string][]GroupTaskAttachment, error) {
	out := map[string][]GroupTaskAttachment{}
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Chunked to stay well under SQLite's bound-parameter limit.
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := queryRows(d, scanGroupTaskAttachment, `SELECT `+groupTaskAttachmentCols+`
			FROM group_task_attachments WHERE comment_id IS NULL AND task_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`) ORDER BY id`, args...)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			out[a.TaskID] = append(out[a.TaskID], a)
		}
	}
	return out, nil
}

// GetGroupTaskAttachment returns one file of task id. Keying on both keeps
// a URL from reaching another task's file by swapping the attachment id.
func GetGroupTaskAttachment(id string, attachmentID int64) (*GroupTaskAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	a, err := scanGroupTaskAttachment(d.QueryRow(`SELECT `+groupTaskAttachmentCols+`
		FROM group_task_attachments WHERE task_id = ? AND id = ?`, id, attachmentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGroupTaskAttachmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GroupTaskAttachmentBlobs lists every blob some task's file names.
func GroupTaskAttachmentBlobs() (map[string]bool, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	shas, err := queryRows(d, func(s rowScanner) (string, error) {
		var sha string
		err := s.Scan(&sha)
		return sha, err
	}, `SELECT DISTINCT sha256 FROM group_task_attachments`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(shas))
	for _, sha := range shas {
		out[sha] = true
	}
	return out, nil
}
