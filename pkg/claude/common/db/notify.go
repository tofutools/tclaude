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
	var notifiedAt dbTimestamp
	err = db.QueryRow(`SELECT notified_at FROM notify_state WHERE session_id = ?`, sessionID).Scan(&notifiedAt)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return notifiedAt.Time(), true, nil
}

// SetNotifyTime records the current time as the last notification time for a session.
func SetNotifyTime(sessionID string) error {
	return setNotifyTimeAt(sessionID, time.Now())
}

func setNotifyTimeAt(sessionID string, notifiedAt time.Time) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO notify_state (session_id, notified_at) VALUES (?, ?)`,
		sessionID, dbTime(notifiedAt))
	return err
}
