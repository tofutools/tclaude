package db

import (
	"database/sql"
	"fmt"
	"time"
)

// SessionRow represents a session row in the database.
type SessionRow struct {
	ID             string
	TmuxSession    string
	PID            int
	Cwd            string
	ConvID         string
	Status         string
	StatusDetail   string
	AutoRegistered bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SaveSession inserts or replaces a session, setting updated_at to now.
func SaveSession(s *SessionRow) error {
	db, err := Open()
	if err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	_, err = db.Exec(`INSERT OR REPLACE INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail, auto_registered, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.TmuxSession, s.PID, s.Cwd, s.ConvID,
		s.Status, s.StatusDetail, boolToInt(s.AutoRegistered),
		s.CreatedAt.Format(time.RFC3339Nano), s.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

// LoadSession loads a session by primary key.
func LoadSession(id string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail,
		auto_registered, created_at, updated_at FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// DeleteSession removes a session by ID.
func DeleteSession(id string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ListSessions returns all sessions.
func ListSessions() ([]*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail,
		auto_registered, created_at, updated_at FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessions(rows)
}

// FindSessionByConvID finds a session by conversation ID using the index.
func FindSessionByConvID(convID string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail,
		auto_registered, created_at, updated_at FROM sessions WHERE conv_id = ? LIMIT 1`, convID)
	s, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// SessionExists checks whether a session with the given ID exists.
func SessionExists(id string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&count)
	return count > 0, err
}

// CleanupOldExited deletes exited sessions older than maxAge and returns the count deleted.
func CleanupOldExited(maxAge time.Duration) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339Nano)
	result, err := db.Exec(`DELETE FROM sessions WHERE status = 'exited' AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MaxUpdatedAt returns the most recent updated_at across all sessions.
// Returns zero time if no sessions exist.
func MaxUpdatedAt() (time.Time, error) {
	db, err := Open()
	if err != nil {
		return time.Time{}, err
	}
	var s sql.NullString
	err = db.QueryRow(`SELECT MAX(updated_at) FROM sessions`).Scan(&s)
	if err != nil || !s.Valid {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, s.String)
}

// scanSession scans a single session row.
func scanSession(row *sql.Row) (*SessionRow, error) {
	var s SessionRow
	var autoReg int
	var createdStr, updatedStr string
	err := row.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
		&s.Status, &s.StatusDetail, &autoReg, &createdStr, &updatedStr)
	if err != nil {
		return nil, err
	}
	s.AutoRegistered = autoReg != 0
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return &s, nil
}

// scanSessions scans multiple session rows.
func scanSessions(rows *sql.Rows) ([]*SessionRow, error) {
	var result []*SessionRow
	for rows.Next() {
		var s SessionRow
		var autoReg int
		var createdStr, updatedStr string
		err := rows.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
			&s.Status, &s.StatusDetail, &autoReg, &createdStr, &updatedStr)
		if err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		s.AutoRegistered = autoReg != 0
		s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
		result = append(result, &s)
	}
	return result, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
