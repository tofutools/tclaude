package db

import (
	"database/sql"
	"time"
)

// GetNotifyTime returns the last notification time for a session.
// Returns zero time and false if no record exists.
func GetNotifyTime(sessionID string) (time.Time, bool, error) {
	db, err := Open()
	if err != nil {
		return time.Time{}, false, err
	}
	var s string
	err = db.QueryRow(`SELECT notified_at FROM notify_state WHERE session_id = ?`, sessionID).Scan(&s)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// SetNotifyTime records the current time as the last notification time for a session.
func SetNotifyTime(sessionID string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO notify_state (session_id, notified_at) VALUES (?, ?)`,
		sessionID, time.Now().Format(time.RFC3339Nano))
	return err
}
