package db

import (
	"database/sql"
	"encoding/json"
	"time"
)

type CodexTelemetryCheckpointRow struct {
	Data         json.RawMessage
	FailureCount int
}

// SaveCodexTelemetryCheckpoint replaces one session's opaque harness follower
// checkpoint. Validation belongs to the harness package; the DB layer cannot
// import it without creating a harness → common/db → harness cycle.
func SaveCodexTelemetryCheckpoint(sessionID string, data json.RawMessage) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO codex_telemetry_checkpoints (session_id, data, failure_count, updated_at)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			data = excluded.data, failure_count = 0, updated_at = excluded.updated_at`,
		sessionID, string(data), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// LoadCodexTelemetryCheckpoint returns one session's opaque checkpoint and its
// consecutive processing-failure count, or nil when no checkpoint exists.
func LoadCodexTelemetryCheckpoint(sessionID string) (*CodexTelemetryCheckpointRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	row := &CodexTelemetryCheckpointRow{}
	err = d.QueryRow(`SELECT data, failure_count FROM codex_telemetry_checkpoints WHERE session_id = ?`, sessionID).
		Scan(&raw, &row.FailureCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.Data = json.RawMessage(raw)
	return row, nil
}

// IncrementCodexTelemetryCheckpointFailures records a genuine follower error.
// Successful saves reset the counter; incomplete trailing records never reach
// this path because the follower treats them as a normal retryable tail.
func IncrementCodexTelemetryCheckpointFailures(sessionID string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var failures int
	err = d.QueryRow(`UPDATE codex_telemetry_checkpoints
		SET failure_count = failure_count + 1, updated_at = ?
		WHERE session_id = ?
		RETURNING failure_count`,
		time.Now().UTC().Format(time.RFC3339Nano), sessionID).Scan(&failures)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return failures, err
}

// DeleteCodexTelemetryCheckpoint drops a malformed or otherwise unusable
// durable checkpoint. A subsequent successful full scan recreates it.
func DeleteCodexTelemetryCheckpoint(sessionID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM codex_telemetry_checkpoints WHERE session_id = ?`, sessionID)
	return err
}
