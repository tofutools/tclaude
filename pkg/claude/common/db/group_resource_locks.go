package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Group resource kinds a lock applies to.
const (
	GroupResourceProcess      = "process"       // a process template, by template id
	GroupResourceDrawingBoard = "drawing_board" // a collab drawing board, by drawing board id
)

// GroupResourceLocked reports whether the operator locked a resource against
// agent changes. An error reads as locked: a lock that cannot be read must
// not let an agent through.
func GroupResourceLocked(kind, id string) bool {
	d, err := Open()
	if err != nil {
		return true
	}
	var one int
	err = d.QueryRow(`SELECT 1 FROM group_resource_locks WHERE kind = ? AND resource_id = ?`, kind, strings.TrimSpace(id)).Scan(&one)
	if err != nil {
		return !errors.Is(err, sql.ErrNoRows)
	}
	return true
}

// GroupResourceLocks lists the locked resources of a kind.
func GroupResourceLocks(kind string) (map[string]bool, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT resource_id FROM group_resource_locks WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetGroupResourceLocked locks or unlocks a resource. Only the operator's
// dashboard calls it.
func SetGroupResourceLocked(kind, id string, locked bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if locked {
		_, err = d.Exec(`INSERT INTO group_resource_locks (kind, resource_id, locked_at) VALUES (?, ?, ?)
			ON CONFLICT(kind, resource_id) DO NOTHING`, kind, id, time.Now().UnixNano())
	} else {
		_, err = d.Exec(`DELETE FROM group_resource_locks WHERE kind = ? AND resource_id = ?`, kind, id)
	}
	return err
}
