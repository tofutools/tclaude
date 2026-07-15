package db

import (
	"database/sql"
	"fmt"
)

func migrateV122toV123(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v122→v123 (agent message attachments): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_message_attachments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL REFERENCES agent_messages(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size_bytes INTEGER NOT NULL,
			storage_path TEXT NOT NULL,
			UNIQUE(message_id, ordinal)
		)`); err != nil {
		return fmt.Errorf("migrate v122→v123 (create agent_message_attachments): %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_message_attachments_message ON agent_message_attachments(message_id, ordinal)`); err != nil {
		return fmt.Errorf("migrate v122→v123 (index): %w", err)
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS operator_agent_messages (
			message_id INTEGER PRIMARY KEY REFERENCES agent_messages(id) ON DELETE CASCADE
		)`); err != nil {
		return fmt.Errorf("migrate v122→v123 (create operator_agent_messages): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 123`); err != nil {
		return fmt.Errorf("migrate v122→v123 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v122→v123 (commit): %w", err)
	}
	return nil
}
