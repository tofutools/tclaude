package db

import (
	"database/sql"
	"fmt"
)

// migrateV136toV137 extends the canonical command audit with typed,
// privacy-bounded managed-pane exit observations. The same migration adds the
// launch-scoped callback binding and lifecycle intent to sessions: these are
// correlation metadata, not a second event store. All columns are additive so
// older audit readers continue to work unchanged.
func migrateV136toV137(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v136→v137: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	columns := []struct {
		table string
		name  string
		type_ string
	}{
		{"audit_log", "event_id", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "related_event_id", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "session_id", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "tmux_session", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "pane_id", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "observer", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "cause_kind", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "observed_process", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "launch_phase", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "exit_code", "INTEGER"},
		{"audit_log", "signal", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "lifecycle_action", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "reason", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "observed_state", "TEXT NOT NULL DEFAULT ''"},
		{"audit_log", "dedup_key", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_intent", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_intent_event_id", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_intent_generation", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_intent_at", "TEXT"},
		{"sessions", "exit_callback_generation", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_callback_token_hash", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_callback_pane_id", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "exit_callback_used_at", "TEXT"},
		{"sessions", "exit_launch_gate_state", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
			column.table, column.name,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v136→v137: probe %s.%s: %w", column.table, column.name, err)
		}
		if have != 0 {
			continue
		}
		if _, err := tx.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " " + column.type_); err != nil {
			return fmt.Errorf("migrate v136→v137: add %s.%s: %w", column.table, column.name, err)
		}
	}
	if _, err := tx.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_log_exit_dedup
			ON audit_log(dedup_key) WHERE dedup_key <> '';
		CREATE INDEX IF NOT EXISTS idx_audit_log_event_id
			ON audit_log(event_id) WHERE event_id <> '';
	`); err != nil {
		return fmt.Errorf("migrate v136→v137: indexes: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 137`); err != nil {
		return fmt.Errorf("migrate v136→v137: version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v136→v137: commit: %w", err)
	}
	return nil
}
