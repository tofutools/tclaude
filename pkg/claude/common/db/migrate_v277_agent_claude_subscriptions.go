package db

import (
	"database/sql"
	"fmt"
)

// migrateV276toV277 records which agents run on a peer operator's Claude
// subscription. Only the marker is stored: the token itself stays in daemon
// memory, and a relaunch without it refuses instead of using this node's login.
func migrateV276toV277(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v276→v277 (agent claude subscriptions): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS agent_claude_subscriptions (
			agent_id   TEXT PRIMARY KEY,
			peer       TEXT NOT NULL,
			created_at INTEGER NOT NULL
		) STRICT`,
		`UPDATE schema_version SET version = 277`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v276→v277 (agent claude subscriptions): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v276→v277 (commit): %w", err)
	}
	return nil
}
