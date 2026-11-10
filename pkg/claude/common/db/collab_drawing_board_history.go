package db

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Drawing board undo history. Steps live here rather than in the editor so undo
// survives a reload and is the same in every editor the actor has open. They
// are kept per actor, so undoing never reverts someone else's work, and an
// undo is an ordinary new version of each element, which other editors merge.

const (
	collabDrawingBoardHistoryUndo     = "undo"
	collabDrawingBoardHistoryRedo     = "redo"
	collabDrawingBoardHistoryMaxSteps = 100
	collabDrawingBoardHistoryMaxBytes = 16 << 20
)

// CollabDrawingBoardStepElement is one element of a step: what to put back, and a
// fingerprint of what the step left there. An element that no longer matches
// has been changed by someone else, even if the operator's own undo wrote it.
type CollabDrawingBoardStepElement struct {
	ID     string          `json:"id"`
	Before json.RawMessage `json:"before"`
	After  string          `json:"after,omitempty"`
}

// CollabDrawingBoardHistory is what an edit adds to its actor's undo history.
type CollabDrawingBoardHistory struct {
	Actor string
	Steps [][]CollabDrawingBoardStepElement
}

// CollabDrawingBoardRestore turns one step element into the write that undoes it and
// that write's fingerprint, or nil to leave the element alone. cur is the
// stored element, nil if none.
type CollabDrawingBoardRestore func(step CollabDrawingBoardStepElement, cur *CollabDrawingBoardElement) (*CollabDrawingBoardElement, string)

// CollabDrawingBoardUndoResult is what an undo or redo did. Elements carry their
// server version. Skipped counts elements restore left alone, and Empty says
// the stack had no step.
type CollabDrawingBoardUndoResult struct {
	Version  int64
	Elements []CollabDrawingBoardElement
	Skipped  int
	Empty    bool
}

func isCollabDrawingBoardStepBeforeNull(before json.RawMessage) bool {
	return len(before) == 0 || string(before) == "null"
}

// pushCollabDrawingBoardStepTx adds a step to a stack and trims the oldest steps past
// the bounds. The newest step always stays, however large.
func pushCollabDrawingBoardStepTx(tx *sql.Tx, drawingBoardID, actor, stack string, step []CollabDrawingBoardStepElement) error {
	body, err := json.Marshal(step)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO collab_drawing_board_history (drawing_board_id, actor, stack, body) VALUES (?, ?, ?, ?)`,
		drawingBoardID, actor, stack, string(body)); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, length(body) FROM collab_drawing_board_history
 WHERE drawing_board_id = ? AND actor = ? AND stack = ? ORDER BY id DESC`, drawingBoardID, actor, stack)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var cutoff int64
	kept, bytes := 0, 0
	for rows.Next() {
		var id int64
		var size int
		if err := rows.Scan(&id, &size); err != nil {
			return err
		}
		kept++
		bytes += size
		if kept > 1 && (kept > collabDrawingBoardHistoryMaxSteps || bytes > collabDrawingBoardHistoryMaxBytes) {
			cutoff = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	if cutoff == 0 {
		return nil
	}
	_, err = tx.Exec(`DELETE FROM collab_drawing_board_history WHERE drawing_board_id = ? AND actor = ? AND stack = ? AND id <= ?`,
		drawingBoardID, actor, stack, cutoff)
	return err
}

// recordCollabDrawingBoardStepsTx adds new work to the actor's history. Only elements
// the merge accepted count: a resent batch, or an edit that lost to a newer
// version, changed nothing to undo. New work ends what could be redone.
func recordCollabDrawingBoardStepsTx(tx *sql.Tx, drawingBoardID string, history *CollabDrawingBoardHistory, accepted map[string]bool) error {
	if history == nil {
		return nil
	}
	steps := make([][]CollabDrawingBoardStepElement, 0, len(history.Steps))
	for _, step := range history.Steps {
		kept := make([]CollabDrawingBoardStepElement, 0, len(step))
		for _, each := range step {
			if accepted[each.ID] {
				kept = append(kept, each)
			}
		}
		if len(kept) > 0 {
			steps = append(steps, kept)
		}
	}
	if len(steps) == 0 {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM collab_drawing_board_history WHERE drawing_board_id = ? AND actor = ? AND stack = ?`,
		drawingBoardID, history.Actor, collabDrawingBoardHistoryRedo); err != nil {
		return err
	}
	for _, step := range steps {
		if err := pushCollabDrawingBoardStepTx(tx, drawingBoardID, history.Actor, collabDrawingBoardHistoryUndo, step); err != nil {
			return err
		}
	}
	return nil
}

// UndoCollabDrawingBoard takes the actor's newest undo step (or redo step) and writes
// it back as new element versions, moving its inverse to the other stack. A
// write the drawing board cannot hold fails and leaves the step where it was.
func UndoCollabDrawingBoard(id, actor string, redo bool, restore CollabDrawingBoardRestore, maxLive, maxStored int) (CollabDrawingBoardUndoResult, error) {
	var out CollabDrawingBoardUndoResult
	from, to := collabDrawingBoardHistoryUndo, collabDrawingBoardHistoryRedo
	if redo {
		from, to = to, from
	}
	collabDrawingBoardWriteMu.Lock()
	defer collabDrawingBoardWriteMu.Unlock()
	d, err := Open()
	if err != nil {
		return out, err
	}
	tx, err := d.Begin()
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(`SELECT version FROM collab_drawing_boards WHERE id = ?`, id).Scan(&out.Version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrCollabDrawingBoardNotFound
		}
		return out, err
	}
	var stepID int64
	var body string
	err = tx.QueryRow(`SELECT id, body FROM collab_drawing_board_history
 WHERE drawing_board_id = ? AND actor = ? AND stack = ? ORDER BY id DESC LIMIT 1`, id, actor, from).Scan(&stepID, &body)
	if errors.Is(err, sql.ErrNoRows) {
		out.Empty = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if _, err := tx.Exec(`DELETE FROM collab_drawing_board_history WHERE id = ?`, stepID); err != nil {
		return out, err
	}
	var step []CollabDrawingBoardStepElement
	if err := json.Unmarshal([]byte(body), &step); err != nil {
		return out, err
	}
	var writes []CollabDrawingBoardElement
	var inverse []CollabDrawingBoardStepElement
	for _, each := range step {
		var cur *CollabDrawingBoardElement
		var stored CollabDrawingBoardElement
		var deleted int
		var raw string
		err := tx.QueryRow(`SELECT element_version, nonce, is_deleted, body FROM collab_drawing_board_elements WHERE drawing_board_id = ? AND element_id = ?`,
			id, each.ID).Scan(&stored.Version, &stored.Nonce, &deleted, &raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if isCollabDrawingBoardStepBeforeNull(each.Before) {
				continue
			}
		case err != nil:
			return out, err
		default:
			stored.ID, stored.Deleted, stored.Body = each.ID, deleted != 0, json.RawMessage(raw)
			cur = &stored
		}
		write, after := restore(each, cur)
		if write == nil {
			out.Skipped++
			continue
		}
		writes = append(writes, *write)
		var before json.RawMessage
		if cur != nil {
			before = cur.Body
		}
		inverse = append(inverse, CollabDrawingBoardStepElement{ID: each.ID, Before: before, After: after})
	}
	if len(writes) > 0 {
		version, _, err := applyCollabDrawingBoardElementsTx(tx, id, actor, out.Version, writes, maxLive, maxStored)
		if err != nil {
			return out, err
		}
		out.Version = version
		for i := range writes {
			writes[i].ServerVersion = version
		}
		out.Elements = writes
		if err := pushCollabDrawingBoardStepTx(tx, id, actor, to, inverse); err != nil {
			return out, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CollabDrawingBoardUndoResult{}, err
	}
	return out, nil
}
