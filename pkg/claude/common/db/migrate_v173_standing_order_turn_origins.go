package db

import (
	"database/sql"
	"fmt"
)

// migrateV172toV173 adds the durable handshake that marks a queued OpenCode
// standing-order message as the origin of one model turn. The projector uses
// it to suppress standing orders during that turn, preventing prompt/tool
// automations from recursively triggering themselves.
func migrateV172toV173(d *sql.DB) error {
	if schemaVersion(d) >= 173 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v172→v173 (standing-order turn origins): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_standing_order_messages (
			message_id     INTEGER PRIMARY KEY
			               REFERENCES agent_messages(id) ON DELETE CASCADE,
			order_id       INTEGER NOT NULL,
			order_revision INTEGER NOT NULL,
			opencode_message_id TEXT NOT NULL UNIQUE
		);
		CREATE TABLE IF NOT EXISTS agent_standing_order_turn_origins (
			target_agent TEXT PRIMARY KEY,
			target_conv  TEXT NOT NULL,
			message_id   INTEGER NOT NULL CHECK(message_id > 0),
			opencode_message_id TEXT NOT NULL,
			state        TEXT NOT NULL CHECK(state IN ('pending', 'active')),
			armed_at     TEXT NOT NULL,
			expires_at   TEXT NOT NULL
		);
		UPDATE schema_version SET version = 173;
	`); err != nil {
		return fmt.Errorf("migrate v172→v173 (standing-order turn origins): apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v172→v173 (standing-order turn origins): commit: %w", err)
	}
	return nil
}
