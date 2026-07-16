package db

import (
	"database/sql"
	"fmt"
)

// migrateV129toV130 separates durable regular-message processing from tmux
// notification delivery. A regular inbox row can therefore remain unread and
// count toward backpressure even when its offline notification was discarded.
func migrateV129toV130(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v129→v130 (regular message state): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveMessages, err := txTableExists(tx, "agent_messages")
	if err != nil {
		return fmt.Errorf("migrate v129→v130 (probe agent_messages): %w", err)
	}
	if haveMessages {
		columns := []struct {
			name string
			sql  string
		}{
			{"regular_send", `ALTER TABLE agent_messages ADD COLUMN regular_send INTEGER NOT NULL DEFAULT 0`},
			{"started_at", `ALTER TABLE agent_messages ADD COLUMN started_at TEXT NOT NULL DEFAULT ''`},
			{"processed_at", `ALTER TABLE agent_messages ADD COLUMN processed_at TEXT NOT NULL DEFAULT ''`},
			{"nudge_discarded_at", `ALTER TABLE agent_messages ADD COLUMN nudge_discarded_at TEXT NOT NULL DEFAULT ''`},
		}
		for _, column := range columns {
			if err := addColumnIfMissing(tx, "agent_messages", column.name, column.sql); err != nil {
				return fmt.Errorf("migrate v129→v130 (add %s): %w", column.name, err)
			}
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_messages_regular_agent_backlog
			ON agent_messages(to_agent, regular_send, processed_at) WHERE pin_gen = 0`); err != nil {
			return fmt.Errorf("migrate v129→v130 (agent backlog index): %w", err)
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_messages_regular_conv_backlog
			ON agent_messages(to_conv, regular_send, processed_at)`); err != nil {
			return fmt.Errorf("migrate v129→v130 (conv backlog index): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 130`); err != nil {
		return fmt.Errorf("migrate v129→v130 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v129→v130 (regular message state): commit: %w", err)
	}
	return nil
}
