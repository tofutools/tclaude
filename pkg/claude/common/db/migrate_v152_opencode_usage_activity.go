package db

import (
	"database/sql"
	"fmt"
)

// migrateV151toV152 adds a retained, supported-surface activity index for
// OpenCode assistant messages. OpenCode does not export provider-account quota
// history, so the Usage tab needs this independent record to qualify native
// provider graphs when OpenCode used that provider during the selected span.
// It also preserves provider/model identity when the live session is pruned.
func migrateV151toV152(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v151→v152 (OpenCode usage activity): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS opencode_usage_activity (
			session_id  TEXT NOT NULL,
			message_id  TEXT NOT NULL,
			conv_id     TEXT NOT NULL DEFAULT '',
			provider_id TEXT NOT NULL,
			model_id    TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			PRIMARY KEY (session_id, message_id)
		);
		CREATE INDEX IF NOT EXISTS idx_opencode_usage_activity_observed
			ON opencode_usage_activity(observed_at, provider_id);
	`); err != nil {
		return fmt.Errorf("migrate v151→v152 (OpenCode usage activity): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 152`); err != nil {
		return fmt.Errorf("migrate v151→v152 (OpenCode usage activity): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v151→v152 (OpenCode usage activity): commit: %w", err)
	}
	return nil
}
