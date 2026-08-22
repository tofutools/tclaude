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
