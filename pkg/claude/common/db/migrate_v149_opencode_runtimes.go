package db

import (
	"database/sql"
	"fmt"
)

// migrateV148toV149 adds the private runtime registry for agentd-owned
// OpenCode servers. A server is started before `session new` writes its
// sessions row, so this table intentionally has no foreign key: the reaper
// removes records whose pane never materialises or whose session was deleted.
//
// The password is private control-plane state. It lives in the same protected
// SQLite database as the rest of agentd's private state and never enters a
// pane command or process argv.
func migrateV148toV149(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v148→v149 (OpenCode runtimes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS opencode_runtimes (
			session_id TEXT PRIMARY KEY,
			conv_id    TEXT NOT NULL DEFAULT '',
			server_url TEXT NOT NULL,
			password   TEXT NOT NULL,
			pid        INTEGER NOT NULL DEFAULT 0,
			cwd        TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("migrate v148→v149 (OpenCode runtimes): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 149`); err != nil {
		return fmt.Errorf("migrate v148→v149 (OpenCode runtimes): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v148→v149 (OpenCode runtimes): commit: %w", err)
	}
	return nil
}
