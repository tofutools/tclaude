package db

import (
	"database/sql"
	"errors"
	"time"
)

// A drawing board session is the drawing board's equivalent of a tmux session: a durable
// record that an agent is working on a drawing board, with or without a dashboard open
// on it. There is no idle expiry. A session ends when its agent closes it, the
// agent retires (the trigger in migrateV288toV289, so every retire path counts),
// the drawing board is deleted, or the operator ends it.

var (
	// ErrCollabDrawingBoardNoSession reports closing a session the agent does not have.
	ErrCollabDrawingBoardNoSession = errors.New("no session on this drawing board")
	// ErrCollabDrawingBoardAgentRetired reports opening a session for a retired agent.
	ErrCollabDrawingBoardAgentRetired = errors.New("agent is retired")
)

// CollabDrawingBoardSession is one (drawing board, agent) record, with what the Background
// terminals listing shows of each side.
type CollabDrawingBoardSession struct {
	DrawingBoardID    string
	DrawingBoardTitle string
	AgentID           string
	ConvID            string // the agent's current generation
	StartedAt         time.Time
	LastActiveAt      time.Time
}

// refreshCollabDrawingBoardSessionTx starts the agent's session on the drawing board, or
// marks it active. A retired agent gets none, so an edit that was already in
// flight when it retired cannot bring a session back; ok reports which.
func refreshCollabDrawingBoardSessionTx(tx *sql.Tx, drawingBoardID, agentID string) (ok bool, err error) {
	now := dbTime(time.Now())
	res, err := tx.Exec(`INSERT INTO collab_drawing_board_sessions (drawing_board_id, agent_id, started_at, last_active_at)
 SELECT ?, a.agent_id, ?, ? FROM agents a WHERE a.agent_id = ? AND a.retired_at IS NULL
 ON CONFLICT(drawing_board_id, agent_id) DO UPDATE SET last_active_at = excluded.last_active_at`,
		drawingBoardID, now, now, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// OpenCollabDrawingBoardSession (re)starts the agent's session on the drawing board and makes
// it a participant, so the operator's drawing board activity reaches it.
func OpenCollabDrawingBoardSession(drawingBoardID, agentID string) (*CollabDrawingBoard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM collab_drawing_boards WHERE id = ?`, drawingBoardID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCollabDrawingBoardNotFound
		}
		return nil, err
	}
	ok, err := refreshCollabDrawingBoardSessionTx(tx, drawingBoardID, agentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCollabDrawingBoardAgentRetired
	}
	if err := addCollabDrawingBoardParticipantTx(tx, drawingBoardID, agentID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetCollabDrawingBoard(drawingBoardID)
}

// CloseCollabDrawingBoardSession ends one agent's session on the drawing board. The agent
// stays a participant.
func CloseCollabDrawingBoardSession(drawingBoardID, agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM collab_drawing_board_sessions WHERE drawing_board_id = ? AND agent_id = ?`, drawingBoardID, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := GetCollabDrawingBoard(drawingBoardID); err != nil {
		return err
	}
	return ErrCollabDrawingBoardNoSession
}

// EndCollabDrawingBoardSessions ends every session on the drawing board, returning how many
// there were. The drawing board is untouched.
func EndCollabDrawingBoardSessions(drawingBoardID string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM collab_drawing_board_sessions WHERE drawing_board_id = ?`, drawingBoardID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListCollabDrawingBoardSessions returns every session, a drawing board's together, its
// longest-running agent first.
func ListCollabDrawingBoardSessions() ([]CollabDrawingBoardSession, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT s.drawing_board_id, b.title, s.agent_id, a.current_conv_id, s.started_at, s.last_active_at
 FROM collab_drawing_board_sessions s
 JOIN collab_drawing_boards b ON b.id = s.drawing_board_id
 JOIN agents a ON a.agent_id = s.agent_id
 ORDER BY s.drawing_board_id, s.started_at, s.agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollabDrawingBoardSession{}
	for rows.Next() {
		var s CollabDrawingBoardSession
		var started, active dbTimestamp
		if err := rows.Scan(&s.DrawingBoardID, &s.DrawingBoardTitle, &s.AgentID, &s.ConvID, &started, &active); err != nil {
			return nil, err
		}
		s.StartedAt, s.LastActiveAt = started.Time(), active.Time()
		out = append(out, s)
	}
	return out, rows.Err()
}
