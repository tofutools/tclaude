package db

import (
	"database/sql"
	"fmt"
)

// migrateV115toV116 adds agent-published files to the human notification
// channel. The bytes stay in agentd's private data directory; this table only
// records the metadata and opaque daemon-owned path used by the download route.
func migrateV115toV116(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v115→v116 (human message attachments): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS human_message_attachments (
			message_id   INTEGER PRIMARY KEY REFERENCES human_messages(id) ON DELETE CASCADE,
			filename     TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size_bytes   INTEGER NOT NULL,
			storage_path TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("migrate v115→v116 (create human_message_attachments): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 116`); err != nil {
		return fmt.Errorf("migrate v115→v116 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v115→v116 (commit): %w", err)
	}
	return nil
}
