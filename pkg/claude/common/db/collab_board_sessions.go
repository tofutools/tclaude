package db

import (
	"database/sql"
	"errors"
	"time"
)

// A board session is the board's equivalent of a tmux session: a durable
// record that an agent is working on a board, with or without a dashboard open
// on it. There is no idle expiry. A session ends when its agent closes it, the
// agent retires (the trigger in migrateV288toV289, so every retire path counts),
// the board is deleted, or the operator ends it.

var (
	// ErrCollabBoardNoSession reports closing a session the agent does not have.
	ErrCollabBoardNoSession = errors.New("no session on this board")
	// ErrCollabBoardAgentRetired reports opening a session for a retired agent.
	ErrCollabBoardAgentRetired = errors.New("agent is retired")
)

// CollabBoardSession is one (board, agent) record, with what the Background
// terminals listing shows of each side.
type CollabBoardSession struct {
	BoardID      string
	BoardTitle   string
	AgentID      string
	ConvID       string // the agent's current generation
	StartedAt    time.Time
	LastActiveAt time.Time
}

// refreshCollabBoardSessionTx starts the agent's session on the board, or
// marks it active. A retired agent gets none, so an edit that was already in
// flight when it retired cannot bring a session back; ok reports which.
func refreshCollabBoardSessionTx(tx *sql.Tx, boardID, agentID string) (ok bool, err error) {
	now := dbTime(time.Now())
	res, err := tx.Exec(`INSERT INTO collab_board_sessions (board_id, agent_id, started_at, last_active_at)
 SELECT ?, a.agent_id, ?, ? FROM agents a WHERE a.agent_id = ? AND a.retired_at IS NULL
 ON CONFLICT(board_id, agent_id) DO UPDATE SET last_active_at = excluded.last_active_at`,
		boardID, now, now, agentID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// OpenCollabBoardSession (re)starts the agent's session on the board and makes
// it a participant, so the operator's board activity reaches it.
func OpenCollabBoardSession(boardID, agentID string) (*CollabBoard, error) {
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
	if err := tx.QueryRow(`SELECT 1 FROM collab_boards WHERE id = ?`, boardID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCollabBoardNotFound
		}
		return nil, err
	}
	ok, err := refreshCollabBoardSessionTx(tx, boardID, agentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCollabBoardAgentRetired
	}
	if err := addCollabBoardParticipantTx(tx, boardID, agentID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetCollabBoard(boardID)
}

// CloseCollabBoardSession ends one agent's session on the board. The agent
// stays a participant.
func CloseCollabBoardSession(boardID, agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM collab_board_sessions WHERE board_id = ? AND agent_id = ?`, boardID, agentID)
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
	if _, err := GetCollabBoard(boardID); err != nil {
		return err
	}
	return ErrCollabBoardNoSession
}

// EndCollabBoardSessions ends every session on the board, returning how many
// there were. The board is untouched.
func EndCollabBoardSessions(boardID string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM collab_board_sessions WHERE board_id = ?`, boardID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ListCollabBoardSessions returns every session, a board's together, its
// longest-running agent first.
func ListCollabBoardSessions() ([]CollabBoardSession, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT s.board_id, b.title, s.agent_id, a.current_conv_id, s.started_at, s.last_active_at
 FROM collab_board_sessions s
 JOIN collab_boards b ON b.id = s.board_id
 JOIN agents a ON a.agent_id = s.agent_id
 ORDER BY s.board_id, s.started_at, s.agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollabBoardSession{}
	for rows.Next() {
		var s CollabBoardSession
		var started, active dbTimestamp
		if err := rows.Scan(&s.BoardID, &s.BoardTitle, &s.AgentID, &s.ConvID, &started, &active); err != nil {
			return nil, err
		}
		s.StartedAt, s.LastActiveAt = started.Time(), active.Time()
		out = append(out, s)
	}
	return out, rows.Err()
}
