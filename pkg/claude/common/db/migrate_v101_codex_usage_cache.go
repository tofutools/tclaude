package db

import (
	"database/sql"
	"fmt"
)

// migrateV100toV101 adds codex_usage_cache: the shared, single-row cache for
// Codex account rate-limit snapshots. Codex exposes 5h/weekly usage in local
// rollout token_count events rather than through tclaude's Claude/Anthropic
// usage_cache path, and hook callbacks run in separate processes from agentd,
// so they need a durable handoff.
func migrateV100toV101(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v100->v101: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS codex_usage_cache (
			id          INTEGER PRIMARY KEY,
			data        TEXT NOT NULL DEFAULT '{}',
			observed_at TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL DEFAULT '',
			source      TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return fmt.Errorf("migrate v100->v101 (create codex_usage_cache): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 101`); err != nil {
		return fmt.Errorf("migrate v100->v101 (version): %w", err)
	}
	return tx.Commit()
}
