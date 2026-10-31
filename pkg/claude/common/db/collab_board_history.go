package db

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Board undo history. Steps live here rather than in the editor so undo
// survives a reload and is the same in every editor the actor has open. They
// are kept per actor, so undoing never reverts someone else's work, and an
// undo is an ordinary new version of each element, which other editors merge.

const (
	collabBoardHistoryUndo     = "undo"
	collabBoardHistoryRedo     = "redo"
	collabBoardHistoryMaxSteps = 100
	collabBoardHistoryMaxBytes = 16 << 20
)

// CollabBoardStepElement is one element of a step: what to put back, and a
// fingerprint of what the step left there. An element that no longer matches
// has been changed by someone else, even if the operator's own undo wrote it.
type CollabBoardStepElement struct {
	ID     string          `json:"id"`
	Before json.RawMessage `json:"before"`
	After  string          `json:"after,omitempty"`
}

// CollabBoardHistory is what an edit adds to its actor's undo history.
type CollabBoardHistory struct {
	Actor string
	Steps [][]CollabBoardStepElement
}

// CollabBoardRestore turns one step element into the write that undoes it and
// that write's fingerprint, or nil to leave the element alone. cur is the
// stored element, nil if none.
type CollabBoardRestore func(step CollabBoardStepElement, cur *CollabBoardElement) (*CollabBoardElement, string)

// CollabBoardUndoResult is what an undo or redo did. Elements carry their
// server version. Skipped counts elements restore left alone, and Empty says
// the stack had no step.
type CollabBoardUndoResult struct {
	Version  int64
	Elements []CollabBoardElement
	Skipped  int
	Empty    bool
}

func isCollabBoardStepBeforeNull(before json.RawMessage) bool {
	return len(before) == 0 || string(before) == "null"
}

// pushCollabBoardStepTx adds a step to a stack and trims the oldest steps past
// the bounds. The newest step always stays, however large.
func pushCollabBoardStepTx(tx *sql.Tx, boardID, actor, stack string, step []CollabBoardStepElement) error {
	body, err := json.Marshal(step)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO collab_board_history (board_id, actor, stack, body) VALUES (?, ?, ?, ?)`,
		boardID, actor, stack, string(body)); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, length(body) FROM collab_board_history
 WHERE board_id = ? AND actor = ? AND stack = ? ORDER BY id DESC`, boardID, actor, stack)
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
		if kept > 1 && (kept > collabBoardHistoryMaxSteps || bytes > collabBoardHistoryMaxBytes) {
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
	_, err = tx.Exec(`DELETE FROM collab_board_history WHERE board_id = ? AND actor = ? AND stack = ? AND id <= ?`,
		boardID, actor, stack, cutoff)
	return err
}

// recordCollabBoardStepsTx adds new work to the actor's history. Only elements
// the merge accepted count: a resent batch, or an edit that lost to a newer
// version, changed nothing to undo. New work ends what could be redone.
func recordCollabBoardStepsTx(tx *sql.Tx, boardID string, history *CollabBoardHistory, accepted map[string]bool) error {
	if history == nil {
		return nil
	}
	steps := make([][]CollabBoardStepElement, 0, len(history.Steps))
	for _, step := range history.Steps {
		kept := make([]CollabBoardStepElement, 0, len(step))
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
	if _, err := tx.Exec(`DELETE FROM collab_board_history WHERE board_id = ? AND actor = ? AND stack = ?`,
		boardID, history.Actor, collabBoardHistoryRedo); err != nil {
		return err
	}
	for _, step := range steps {
		if err := pushCollabBoardStepTx(tx, boardID, history.Actor, collabBoardHistoryUndo, step); err != nil {
			return err
		}
	}
	return nil
}

// UndoCollabBoard takes the actor's newest undo step (or redo step) and writes
// it back as new element versions, moving its inverse to the other stack. A
// write the board cannot hold fails and leaves the step where it was.
func UndoCollabBoard(id, actor string, redo bool, restore CollabBoardRestore, maxLive, maxStored int) (CollabBoardUndoResult, error) {
	var out CollabBoardUndoResult
	from, to := collabBoardHistoryUndo, collabBoardHistoryRedo
	if redo {
		from, to = to, from
	}
	collabBoardWriteMu.Lock()
	defer collabBoardWriteMu.Unlock()
	d, err := Open()
	if err != nil {
		return out, err
	}
	tx, err := d.Begin()
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(`SELECT version FROM collab_boards WHERE id = ?`, id).Scan(&out.Version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrCollabBoardNotFound
		}
		return out, err
	}
	var stepID int64
	var body string
	err = tx.QueryRow(`SELECT id, body FROM collab_board_history
 WHERE board_id = ? AND actor = ? AND stack = ? ORDER BY id DESC LIMIT 1`, id, actor, from).Scan(&stepID, &body)
	if errors.Is(err, sql.ErrNoRows) {
		out.Empty = true
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if _, err := tx.Exec(`DELETE FROM collab_board_history WHERE id = ?`, stepID); err != nil {
		return out, err
	}
	var step []CollabBoardStepElement
	if err := json.Unmarshal([]byte(body), &step); err != nil {
		return out, err
	}
	var writes []CollabBoardElement
	var inverse []CollabBoardStepElement
	for _, each := range step {
		var cur *CollabBoardElement
		var stored CollabBoardElement
		var deleted int
		var raw string
		err := tx.QueryRow(`SELECT element_version, nonce, is_deleted, body FROM collab_board_elements WHERE board_id = ? AND element_id = ?`,
			id, each.ID).Scan(&stored.Version, &stored.Nonce, &deleted, &raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if isCollabBoardStepBeforeNull(each.Before) {
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
		inverse = append(inverse, CollabBoardStepElement{ID: each.ID, Before: before, After: after})
	}
	if len(writes) > 0 {
		version, _, err := applyCollabBoardElementsTx(tx, id, actor, out.Version, writes, maxLive, maxStored)
		if err != nil {
			return out, err
		}
		out.Version = version
		for i := range writes {
			writes[i].ServerVersion = version
		}
		out.Elements = writes
		if err := pushCollabBoardStepTx(tx, id, actor, to, inverse); err != nil {
			return out, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CollabBoardUndoResult{}, err
	}
	return out, nil
}
