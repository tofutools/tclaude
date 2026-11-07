package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Collab drawing boards (tcl-4f86kp). The store keeps each element's normalized JSON
// body next to the fields the merge needs; validation and normalization of
// the Excalidraw subset is the daemon's job (agentd/drawing-boards.go).

var (
	ErrCollabDrawingBoardNotFound = errors.New("drawing board not found")
	ErrCollabDrawingBoardExists   = errors.New("drawing board already exists")
	ErrCollabDrawingBoardLimit    = errors.New("too many drawing boards")
)

// CollabDrawingBoardTooManyElementsError reports a patch that would leave more live
// elements on the drawing board than its limit; the whole patch is rolled back.
type CollabDrawingBoardTooManyElementsError struct {
	Count, Limit int
	Stored       bool // the limit on stored rows (tombstones included), not live elements
}

func (e *CollabDrawingBoardTooManyElementsError) Error() string {
	if e.Stored {
		return fmt.Sprintf("drawing board would store %d elements including deletions; the limit is %d", e.Count, e.Limit)
	}
	return fmt.Sprintf("drawing board would hold %d live elements; the limit is %d", e.Count, e.Limit)
}

type CollabDrawingBoard struct {
	ID              string
	Title           string
	Version         int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	UpdatedBy       string
	HasSnapshot     bool
	SnapshotVersion int64
	SnapshotAt      time.Time
	ElementCount    int // live (non-deleted) elements
	Participants    []string
	GroupID         int64  // 0 = no group
	GroupName       string // "" when unlinked or the group is archived
}

type CollabDrawingBoardElement struct {
	ID            string
	Version       int64
	Nonce         int64
	ServerVersion int64
	Deleted       bool
	Body          json.RawMessage
}

// collabDrawingBoardWriteMu serializes element merges so the read-decide-write of
// each element and the drawing board version bump see one consistent drawing board.
var collabDrawingBoardWriteMu sync.Mutex

const collabDrawingBoardColumns = `b.id, b.title, b.version, b.created_at, b.updated_at, b.updated_by,
 b.snapshot_version, b.snapshot_at,
 (SELECT COUNT(*) FROM collab_drawing_board_elements e WHERE e.drawing_board_id = b.id AND e.is_deleted = 0),
 COALESCE(b.group_id, 0),
 COALESCE((SELECT g.name FROM agent_groups g WHERE g.id = b.group_id AND g.archived_at IS NULL), '')`

func scanCollabDrawingBoard(row interface{ Scan(...any) error }) (*CollabDrawingBoard, error) {
	var b CollabDrawingBoard
	var created, updated, snapAt dbTimestamp
	var snapVersion sql.NullInt64
	if err := row.Scan(&b.ID, &b.Title, &b.Version, &created, &updated, &b.UpdatedBy,
		&snapVersion, &snapAt, &b.ElementCount, &b.GroupID, &b.GroupName); err != nil {
		return nil, err
	}
	b.CreatedAt, b.UpdatedAt, b.SnapshotAt = created.Time(), updated.Time(), snapAt.Time()
	b.HasSnapshot, b.SnapshotVersion = snapVersion.Valid, snapVersion.Int64
	return &b, nil
}

func collabDrawingBoardParticipantsTx(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, drawingBoardID string) ([]string, error) {
	rows, err := q.Query(`SELECT agent_id FROM collab_drawing_board_participants WHERE drawing_board_id = ? ORDER BY added_at, agent_id`, drawingBoardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CreateCollabDrawingBoard creates an empty drawing board; maxDrawingBoards bounds how many exist.
func CreateCollabDrawingBoard(id, title, by string, maxDrawingBoards int) (*CollabDrawingBoard, error) {
	if id == "" || title == "" {
		return nil, errors.New("drawing board id and title required")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM collab_drawing_boards`).Scan(&count); err != nil {
		return nil, err
	}
	now := time.Now()
	res, err := tx.Exec(`INSERT INTO collab_drawing_boards (id, title, version, created_at, updated_at, updated_by)
 VALUES (?, ?, 0, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, title, dbTime(now), dbTime(now), by)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrCollabDrawingBoardExists
	}
	if count >= maxDrawingBoards {
		return nil, ErrCollabDrawingBoardLimit
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetCollabDrawingBoard(id)
}

// GetCollabDrawingBoard returns the drawing board with its participants, or
// ErrCollabDrawingBoardNotFound.
func GetCollabDrawingBoard(id string) (*CollabDrawingBoard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	b, err := scanCollabDrawingBoard(d.QueryRow(`SELECT `+collabDrawingBoardColumns+` FROM collab_drawing_boards b WHERE b.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCollabDrawingBoardNotFound
	}
	if err != nil {
		return nil, err
	}
	if b.Participants, err = collabDrawingBoardParticipantsTx(d, id); err != nil {
		return nil, err
	}
	return b, nil
}

func ListCollabDrawingBoards() ([]*CollabDrawingBoard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT ` + collabDrawingBoardColumns + ` FROM collab_drawing_boards b ORDER BY b.updated_at DESC, b.id`)
	if err != nil {
		return nil, err
	}
	drawingBoards := []*CollabDrawingBoard{}
	for rows.Next() {
		b, err := scanCollabDrawingBoard(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		drawingBoards = append(drawingBoards, b)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range drawingBoards {
		if b.Participants, err = collabDrawingBoardParticipantsTx(d, b.ID); err != nil {
			return nil, err
		}
	}
	return drawingBoards, nil
}

func DeleteCollabDrawingBoard(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM collab_drawing_boards WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabDrawingBoardNotFound
	}
	// A deleted drawing board's lock goes with it: a new drawing board by the same id
	// starts unlocked.
	_, err = d.Exec(`DELETE FROM group_resource_locks WHERE kind = ? AND resource_id = ?`, GroupResourceDrawingBoard, id)
	return err
}

// ListCollabDrawingBoardElements returns the drawing board's elements in creation order,
// which is their stacking order: an update keeps the row (and its rowid).
// since < 0 selects the live elements only; since >= 0 selects every element
// (tombstones included) whose server_version is above it.
func ListCollabDrawingBoardElements(id string, since int64) ([]CollabDrawingBoardElement, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT element_id, element_version, nonce, server_version, is_deleted, body
 FROM collab_drawing_board_elements WHERE drawing_board_id = ?`
	args := []any{id}
	if since < 0 {
		q += ` AND is_deleted = 0`
	} else {
		q += ` AND server_version > ?`
		args = append(args, since)
	}
	rows, err := d.Query(q+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollabDrawingBoardElement{}
	for rows.Next() {
		var e CollabDrawingBoardElement
		var deleted int64
		var body string
		if err := rows.Scan(&e.ID, &e.Version, &e.Nonce, &e.ServerVersion, &deleted, &body); err != nil {
			return nil, err
		}
		e.Deleted = deleted != 0
		e.Body = json.RawMessage(body)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ApplyCollabDrawingBoardEdit merges elements into the drawing board with Excalidraw's
// reconciliation rule: an incoming element replaces the stored one when there
// is none, when its version is higher, or when the versions tie and its nonce
// is lower. When at least one element is accepted the drawing board version is bumped
// once and every accepted element is stamped with it. participant, when set,
// joins the drawing board in the same transaction, and when anything was accepted its
// drawing board session starts or refreshes there too. The patch is all-or-nothing:
// one that would leave more than maxLive live elements, or more than maxStored
// rows including tombstones, changes nothing.
//
// history, when non-nil, is recorded in the same transaction, so a step
// cannot outlive or miss its edit.
func ApplyCollabDrawingBoardEdit(id, by, participant string, elems []CollabDrawingBoardElement, history *CollabDrawingBoardHistory, maxLive, maxStored int) (version int64, accepted int, err error) {
	collabDrawingBoardWriteMu.Lock()
	defer collabDrawingBoardWriteMu.Unlock()
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(`SELECT version FROM collab_drawing_boards WHERE id = ?`, id).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrCollabDrawingBoardNotFound
		}
		return 0, 0, err
	}
	version, kept, err := applyCollabDrawingBoardElementsTx(tx, id, by, version, elems, maxLive, maxStored)
	if err != nil {
		return 0, 0, err
	}
	accepted = len(kept)
	if err := recordCollabDrawingBoardStepsTx(tx, id, history, kept); err != nil {
		return 0, 0, err
	}
	if participant != "" {
		if err := addCollabDrawingBoardParticipantTx(tx, id, participant); err != nil {
			return 0, 0, err
		}
	}
	if participant != "" && accepted > 0 {
		if _, err := refreshCollabDrawingBoardSessionTx(tx, id, participant); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return version, accepted, nil
}

// applyCollabDrawingBoardElementsTx is the merge, returning the drawing board's version and
// the ids it accepted. version is the drawing board's current version; the caller
// holds collabDrawingBoardWriteMu.
func applyCollabDrawingBoardElementsTx(tx *sql.Tx, id, by string, version int64, elems []CollabDrawingBoardElement, maxLive, maxStored int) (int64, map[string]bool, error) {
	accepted := map[string]bool{}
	next := version + 1
	// The last occurrence of an id in one patch is the one that counts, as if
	// the patch had been sent element by element.
	winners := map[string]CollabDrawingBoardElement{}
	order := []string{}
	for _, e := range elems {
		cur, seen := winners[e.ID]
		if !seen {
			order = append(order, e.ID)
		}
		if !seen || e.Version > cur.Version || (e.Version == cur.Version && e.Nonce < cur.Nonce) {
			winners[e.ID] = e
		}
	}
	for _, elemID := range order {
		e := winners[elemID]
		var storedVersion, storedNonce int64
		err := tx.QueryRow(`SELECT element_version, nonce FROM collab_drawing_board_elements WHERE drawing_board_id = ? AND element_id = ?`,
			id, e.ID).Scan(&storedVersion, &storedNonce)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return 0, nil, err
		case e.Version > storedVersion, e.Version == storedVersion && e.Nonce < storedNonce:
		default:
			continue
		}
		deleted := 0
		if e.Deleted {
			deleted = 1
		}
		if _, err := tx.Exec(`INSERT INTO collab_drawing_board_elements (drawing_board_id, element_id, element_version, nonce, server_version, is_deleted, body)
 VALUES (?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(drawing_board_id, element_id) DO UPDATE SET element_version = excluded.element_version, nonce = excluded.nonce,
 server_version = excluded.server_version, is_deleted = excluded.is_deleted, body = excluded.body`,
			id, e.ID, e.Version, e.Nonce, next, deleted, string(e.Body)); err != nil {
			return 0, nil, err
		}
		accepted[e.ID] = true
	}
	if len(accepted) > 0 {
		var live, stored int
		if err := tx.QueryRow(`SELECT COALESCE(SUM(is_deleted = 0), 0), COUNT(*) FROM collab_drawing_board_elements WHERE drawing_board_id = ?`, id).Scan(&live, &stored); err != nil {
			return 0, nil, err
		}
		if live > maxLive {
			return 0, nil, &CollabDrawingBoardTooManyElementsError{Count: live, Limit: maxLive}
		}
		if stored > maxStored {
			return 0, nil, &CollabDrawingBoardTooManyElementsError{Count: stored, Limit: maxStored, Stored: true}
		}
		if _, err := tx.Exec(`UPDATE collab_drawing_boards SET version = ?, updated_at = ?, updated_by = ? WHERE id = ?`,
			next, dbTime(time.Now()), by, id); err != nil {
			return 0, nil, err
		}
		version = next
	}
	return version, accepted, nil
}

func addCollabDrawingBoardParticipantTx(tx *sql.Tx, drawingBoardID, agentID string) error {
	_, err := tx.Exec(`INSERT INTO collab_drawing_board_participants (drawing_board_id, agent_id, added_at) VALUES (?, ?, ?)
 ON CONFLICT(drawing_board_id, agent_id) DO NOTHING`, drawingBoardID, agentID, dbTime(time.Now()))
	return err
}

// SetCollabDrawingBoardParticipant adds or removes agentID and returns the drawing board's
// participants afterwards.
func SetCollabDrawingBoardParticipant(drawingBoardID, agentID string, remove bool) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM collab_drawing_boards WHERE id = ?`, drawingBoardID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCollabDrawingBoardNotFound
		}
		return nil, err
	}
	if remove {
		_, err = tx.Exec(`DELETE FROM collab_drawing_board_participants WHERE drawing_board_id = ? AND agent_id = ?`, drawingBoardID, agentID)
	} else {
		err = addCollabDrawingBoardParticipantTx(tx, drawingBoardID, agentID)
	}
	if err != nil {
		return nil, err
	}
	out, err := collabDrawingBoardParticipantsTx(tx, drawingBoardID)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// SetCollabDrawingBoardGroup links the drawing board to a group, or unlinks it (groupID 0).
func SetCollabDrawingBoardGroup(drawingBoardID string, groupID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	var group any
	if groupID > 0 {
		group = groupID
	}
	res, err := d.Exec(`UPDATE collab_drawing_boards SET group_id = ? WHERE id = ?`, group, drawingBoardID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabDrawingBoardNotFound
	}
	return nil
}

// CollabDrawingBoardGroupMembers returns the active (not retired) agents in the
// drawing board's group, or nothing when the drawing board has no live group.
func CollabDrawingBoardGroupMembers(b *CollabDrawingBoard) ([]string, error) {
	if b.GroupID == 0 || b.GroupName == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT m.agent_id FROM agent_group_members m JOIN agents a ON a.agent_id = m.agent_id
 WHERE m.group_id = ? AND a.retired_at IS NULL ORDER BY m.joined_at, m.agent_id`, b.GroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetCollabDrawingBoardSnapshot records which drawing board version the stored PNG snapshot
// shows. The PNG itself is a file the daemon owns.
func SetCollabDrawingBoardSnapshot(drawingBoardID string, version int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE collab_drawing_boards SET snapshot_version = ?, snapshot_at = ? WHERE id = ?`,
		version, dbTime(time.Now()), drawingBoardID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabDrawingBoardNotFound
	}
	return nil
}

// SetCollabDrawingBoardTitle renames a drawing board.
func SetCollabDrawingBoardTitle(drawingBoardID, title string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE collab_drawing_boards SET title = ? WHERE id = ?`, title, drawingBoardID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabDrawingBoardNotFound
	}
	return nil
}
