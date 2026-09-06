package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func SetSessionExecutionBoundary(sessionID, raw string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("SetSessionExecutionBoundary: session id required")
	}
	if strings.TrimSpace(raw) == "" {
		return errors.New("SetSessionExecutionBoundary: boundary JSON required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`INSERT INTO session_execution_boundaries (session_id, boundary_json)
		VALUES (?, ?) ON CONFLICT(session_id) DO UPDATE SET boundary_json = excluded.boundary_json`,
		sessionID, raw)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return fmt.Errorf("session execution boundary write affected %d rows", n)
	}
	return nil
}

// SetSessionExecutionBoundaryForLaunch publishes a boundary only while the
// durable session row still names the exact live launch. The pane association
// is part of the fence so a delayed server or launch writer cannot populate a
// successor execution that reused the stable session id.
func SetSessionExecutionBoundaryForLaunch(
	sessionID, generation, tmuxSession, paneID, raw string,
) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	generation = strings.TrimSpace(generation)
	tmuxSession = strings.TrimSpace(tmuxSession)
	paneID = strings.TrimSpace(paneID)
	if sessionID == "" || generation == "" || tmuxSession == "" || paneID == "" {
		return false, errors.New("execution boundary launch identity is incomplete")
	}
	if strings.TrimSpace(raw) == "" {
		return false, errors.New("execution boundary JSON required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	result, err := d.Exec(`INSERT INTO session_execution_boundaries (session_id, boundary_json)
		SELECT ?, ? WHERE EXISTS (
			SELECT 1 FROM sessions WHERE id = ? AND exit_callback_generation = ?
				AND tmux_session = ? AND exit_callback_pane_id = ? AND status <> 'exited'
		)
		ON CONFLICT(session_id) DO UPDATE SET boundary_json = excluded.boundary_json
		WHERE EXISTS (
			SELECT 1 FROM sessions WHERE id = ? AND exit_callback_generation = ?
				AND tmux_session = ? AND exit_callback_pane_id = ? AND status <> 'exited'
		)`, sessionID, raw, sessionID, generation, tmuxSession, paneID,
		sessionID, generation, tmuxSession, paneID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func SessionExecutionBoundary(sessionID string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var raw string
	err = d.QueryRow(`SELECT boundary_json FROM session_execution_boundaries WHERE session_id = ?`,
		strings.TrimSpace(sessionID)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
