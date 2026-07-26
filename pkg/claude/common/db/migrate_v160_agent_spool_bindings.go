package db

import (
	"database/sql"
	"fmt"
)

// migrateV159toV160 adds the binding table for the experimental file-spool
// transport: each row maps one unguessable spool directory to the conv it
// was provisioned for at spawn. The daemon stamps caller identity for spool
// requests from this table (possession of the bound directory IS the
// identity), so the binding must live in daemon-trusted storage the agent
// cannot write — never inside the agent-writable spool directory itself.
func migrateV159toV160(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v159→v160 (agent spool bindings): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_spool_bindings (
			spool_id   TEXT PRIMARY KEY,
			conv_id    TEXT NOT NULL,
			dir        TEXT NOT NULL,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_agent_spool_bindings_conv
			ON agent_spool_bindings(conv_id);
	`); err != nil {
		return fmt.Errorf("migrate v159→v160 (agent spool bindings): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 160`); err != nil {
		return fmt.Errorf("migrate v159→v160 (agent spool bindings): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v159→v160 (agent spool bindings): commit: %w", err)
	}
	return nil
}
