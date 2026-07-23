package db

import (
	"database/sql"
	"time"
)

// OpenCodeRuntime is the durable recovery record for one agentd-owned
// `opencode serve`. Password is intentionally omitted from logs and public
// session projections; only the private runtime manager reads this row.
type OpenCodeRuntime struct {
	SessionID string
	ConvID    string
	ServerURL string
	Password  string
	PID       int
	Cwd       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func UpsertOpenCodeRuntime(runtime OpenCodeRuntime) error {
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if runtime.CreatedAt.IsZero() {
		runtime.CreatedAt = now
	}
	runtime.UpdatedAt = now
	_, err = d.Exec(`
		INSERT INTO opencode_runtimes
			(session_id, conv_id, server_url, password, pid, cwd, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			conv_id = excluded.conv_id,
			server_url = excluded.server_url,
			password = excluded.password,
			pid = excluded.pid,
			cwd = excluded.cwd,
			updated_at = excluded.updated_at
	`, runtime.SessionID, runtime.ConvID, runtime.ServerURL, runtime.Password,
		runtime.PID, runtime.Cwd, runtime.CreatedAt.Format(time.RFC3339Nano),
		runtime.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func GetOpenCodeRuntime(sessionID string) (*OpenCodeRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, created_at, updated_at
		FROM opencode_runtimes WHERE session_id = ?
	`, sessionID)
	runtime, err := scanOpenCodeRuntime(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return runtime, err
}

func GetOpenCodeRuntimeByConvID(convID string) (*OpenCodeRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, created_at, updated_at
		FROM opencode_runtimes WHERE conv_id = ? ORDER BY created_at DESC LIMIT 1
	`, convID)
	runtime, err := scanOpenCodeRuntime(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return runtime, err
}

func ListOpenCodeRuntimes() ([]OpenCodeRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, created_at, updated_at
		FROM opencode_runtimes ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runtimes []OpenCodeRuntime
	for rows.Next() {
		runtime, err := scanOpenCodeRuntime(rows)
		if err != nil {
			return nil, err
		}
		runtimes = append(runtimes, *runtime)
	}
	return runtimes, rows.Err()
}

func DeleteOpenCodeRuntime(sessionID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM opencode_runtimes WHERE session_id = ?`, sessionID)
	return err
}

type openCodeRuntimeScanner interface {
	Scan(dest ...any) error
}

func scanOpenCodeRuntime(row openCodeRuntimeScanner) (*OpenCodeRuntime, error) {
	var runtime OpenCodeRuntime
	var created, updated string
	if err := row.Scan(&runtime.SessionID, &runtime.ConvID, &runtime.ServerURL,
		&runtime.Password, &runtime.PID, &runtime.Cwd, &created, &updated); err != nil {
		return nil, err
	}
	runtime.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	runtime.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &runtime, nil
}
