package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Collab boards (tcl-4f86kp). The store keeps each element's normalized JSON
// body next to the fields the merge needs; validation and normalization of
// the Excalidraw subset is the daemon's job (agentd/boards.go).

var (
	ErrCollabBoardNotFound = errors.New("board not found")
	ErrCollabBoardExists   = errors.New("board already exists")
	ErrCollabBoardLimit    = errors.New("too many boards")
)

// CollabBoardTooManyElementsError reports a patch that would leave more live
// elements on the board than its limit; the whole patch is rolled back.
type CollabBoardTooManyElementsError struct {
	Count, Limit int
	Stored       bool // the limit on stored rows (tombstones included), not live elements
}

func (e *CollabBoardTooManyElementsError) Error() string {
	if e.Stored {
		return fmt.Sprintf("board would store %d elements including deletions; the limit is %d", e.Count, e.Limit)
	}
	return fmt.Sprintf("board would hold %d live elements; the limit is %d", e.Count, e.Limit)
}

type CollabBoard struct {
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

type CollabBoardElement struct {
	ID            string
	Version       int64
	Nonce         int64
	ServerVersion int64
	Deleted       bool
	Body          json.RawMessage
}

// collabBoardWriteMu serializes element merges so the read-decide-write of
// each element and the board version bump see one consistent board.
var collabBoardWriteMu sync.Mutex

const collabBoardColumns = `b.id, b.title, b.version, b.created_at, b.updated_at, b.updated_by,
 b.snapshot_version, b.snapshot_at,
 (SELECT COUNT(*) FROM collab_board_elements e WHERE e.board_id = b.id AND e.is_deleted = 0),
 COALESCE(b.group_id, 0),
 COALESCE((SELECT g.name FROM agent_groups g WHERE g.id = b.group_id AND g.archived_at IS NULL), '')`

func scanCollabBoard(row interface{ Scan(...any) error }) (*CollabBoard, error) {
	var b CollabBoard
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

func collabBoardParticipantsTx(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, boardID string) ([]string, error) {
	rows, err := q.Query(`SELECT agent_id FROM collab_board_participants WHERE board_id = ? ORDER BY added_at, agent_id`, boardID)
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

// CreateCollabBoard creates an empty board; maxBoards bounds how many exist.
func CreateCollabBoard(id, title, by string, maxBoards int) (*CollabBoard, error) {
	if id == "" || title == "" {
		return nil, errors.New("board id and title required")
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
	if err := tx.QueryRow(`SELECT COUNT(*) FROM collab_boards`).Scan(&count); err != nil {
		return nil, err
	}
	now := time.Now()
	res, err := tx.Exec(`INSERT INTO collab_boards (id, title, version, created_at, updated_at, updated_by)
 VALUES (?, ?, 0, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, id, title, dbTime(now), dbTime(now), by)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrCollabBoardExists
	}
	if count >= maxBoards {
		return nil, ErrCollabBoardLimit
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetCollabBoard(id)
}

// GetCollabBoard returns the board with its participants, or
// ErrCollabBoardNotFound.
func GetCollabBoard(id string) (*CollabBoard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	b, err := scanCollabBoard(d.QueryRow(`SELECT `+collabBoardColumns+` FROM collab_boards b WHERE b.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCollabBoardNotFound
	}
	if err != nil {
		return nil, err
	}
	if b.Participants, err = collabBoardParticipantsTx(d, id); err != nil {
		return nil, err
	}
	return b, nil
}

func ListCollabBoards() ([]*CollabBoard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT ` + collabBoardColumns + ` FROM collab_boards b ORDER BY b.updated_at DESC, b.id`)
	if err != nil {
		return nil, err
	}
	boards := []*CollabBoard{}
	for rows.Next() {
		b, err := scanCollabBoard(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		boards = append(boards, b)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, b := range boards {
		if b.Participants, err = collabBoardParticipantsTx(d, b.ID); err != nil {
			return nil, err
		}
	}
	return boards, nil
}

func DeleteCollabBoard(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM collab_boards WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabBoardNotFound
	}
	return nil
}

// ListCollabBoardElements returns the board's elements in creation order,
// which is their stacking order: an update keeps the row (and its rowid).
// since < 0 selects the live elements only; since >= 0 selects every element
// (tombstones included) whose server_version is above it.
func ListCollabBoardElements(id string, since int64) ([]CollabBoardElement, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT element_id, element_version, nonce, server_version, is_deleted, body
 FROM collab_board_elements WHERE board_id = ?`
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
	out := []CollabBoardElement{}
	for rows.Next() {
		var e CollabBoardElement
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

// ApplyCollabBoardElements merges elements into the board with Excalidraw's
// reconciliation rule: an incoming element replaces the stored one when there
// is none, when its version is higher, or when the versions tie and its nonce
// is lower. When at least one element is accepted the board version is bumped
// once and every accepted element is stamped with it. participant, when set,
// joins the board in the same transaction. The patch is all-or-nothing: one
// that would leave more than maxLive live elements, or more than maxStored
// rows including tombstones, changes nothing.
func ApplyCollabBoardElements(id, by, participant string, elems []CollabBoardElement, maxLive, maxStored int) (version int64, accepted int, err error) {
	return ApplyCollabBoardEdit(id, by, participant, elems, nil, maxLive, maxStored)
}

// ApplyCollabBoardEdit is ApplyCollabBoardElements that also records history,
// in the same transaction, so a step cannot outlive or miss its edit.
func ApplyCollabBoardEdit(id, by, participant string, elems []CollabBoardElement, history *CollabBoardHistory, maxLive, maxStored int) (version int64, accepted int, err error) {
	collabBoardWriteMu.Lock()
	defer collabBoardWriteMu.Unlock()
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow(`SELECT version FROM collab_boards WHERE id = ?`, id).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrCollabBoardNotFound
		}
		return 0, 0, err
	}
	version, kept, err := applyCollabBoardElementsTx(tx, id, by, version, elems, maxLive, maxStored)
	if err != nil {
		return 0, 0, err
	}
	accepted = len(kept)
	if err := recordCollabBoardStepsTx(tx, id, history, kept); err != nil {
		return 0, 0, err
	}
	if participant != "" {
		if err := addCollabBoardParticipantTx(tx, id, participant); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return version, accepted, nil
}

// applyCollabBoardElementsTx is the merge, returning the board's version and
// the ids it accepted. version is the board's current version; the caller
// holds collabBoardWriteMu.
func applyCollabBoardElementsTx(tx *sql.Tx, id, by string, version int64, elems []CollabBoardElement, maxLive, maxStored int) (int64, map[string]bool, error) {
	accepted := map[string]bool{}
	next := version + 1
	// The last occurrence of an id in one patch is the one that counts, as if
	// the patch had been sent element by element.
	winners := map[string]CollabBoardElement{}
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
		err := tx.QueryRow(`SELECT element_version, nonce FROM collab_board_elements WHERE board_id = ? AND element_id = ?`,
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
		if _, err := tx.Exec(`INSERT INTO collab_board_elements (board_id, element_id, element_version, nonce, server_version, is_deleted, body)
 VALUES (?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(board_id, element_id) DO UPDATE SET element_version = excluded.element_version, nonce = excluded.nonce,
 server_version = excluded.server_version, is_deleted = excluded.is_deleted, body = excluded.body`,
			id, e.ID, e.Version, e.Nonce, next, deleted, string(e.Body)); err != nil {
			return 0, nil, err
		}
		accepted[e.ID] = true
	}
	if len(accepted) > 0 {
		var live, stored int
		if err := tx.QueryRow(`SELECT COALESCE(SUM(is_deleted = 0), 0), COUNT(*) FROM collab_board_elements WHERE board_id = ?`, id).Scan(&live, &stored); err != nil {
			return 0, nil, err
		}
		if live > maxLive {
			return 0, nil, &CollabBoardTooManyElementsError{Count: live, Limit: maxLive}
		}
		if stored > maxStored {
			return 0, nil, &CollabBoardTooManyElementsError{Count: stored, Limit: maxStored, Stored: true}
		}
		if _, err := tx.Exec(`UPDATE collab_boards SET version = ?, updated_at = ?, updated_by = ? WHERE id = ?`,
			next, dbTime(time.Now()), by, id); err != nil {
			return 0, nil, err
		}
		version = next
	}
	return version, accepted, nil
}

func addCollabBoardParticipantTx(tx *sql.Tx, boardID, agentID string) error {
	_, err := tx.Exec(`INSERT INTO collab_board_participants (board_id, agent_id, added_at) VALUES (?, ?, ?)
 ON CONFLICT(board_id, agent_id) DO NOTHING`, boardID, agentID, dbTime(time.Now()))
	return err
}

// SetCollabBoardParticipant adds or removes agentID and returns the board's
// participants afterwards.
func SetCollabBoardParticipant(boardID, agentID string, remove bool) ([]string, error) {
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
	if err := tx.QueryRow(`SELECT 1 FROM collab_boards WHERE id = ?`, boardID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCollabBoardNotFound
		}
		return nil, err
	}
	if remove {
		_, err = tx.Exec(`DELETE FROM collab_board_participants WHERE board_id = ? AND agent_id = ?`, boardID, agentID)
	} else {
		err = addCollabBoardParticipantTx(tx, boardID, agentID)
	}
	if err != nil {
		return nil, err
	}
	out, err := collabBoardParticipantsTx(tx, boardID)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// SetCollabBoardGroup links the board to a group, or unlinks it (groupID 0).
func SetCollabBoardGroup(boardID string, groupID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	var group any
	if groupID > 0 {
		group = groupID
	}
	res, err := d.Exec(`UPDATE collab_boards SET group_id = ? WHERE id = ?`, group, boardID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabBoardNotFound
	}
	return nil
}

// CollabBoardGroupMembers returns the active (not retired) agents in the
// board's group, or nothing when the board has no live group.
func CollabBoardGroupMembers(b *CollabBoard) ([]string, error) {
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

// SetCollabBoardSnapshot records which board version the stored PNG snapshot
// shows. The PNG itself is a file the daemon owns.
func SetCollabBoardSnapshot(boardID string, version int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE collab_boards SET snapshot_version = ?, snapshot_at = ? WHERE id = ?`,
		version, dbTime(time.Now()), boardID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCollabBoardNotFound
	}
	return nil
}
