package db

import (
	"database/sql"
	"fmt"
)

// migrateV272toV273 adds cost_ignored_convs: conversations the operator has
// excluded from every cost total (the dashboard Costs view's per-row Ignore).
// The recorded history in session_cost_daily is left untouched, so restoring
// a conversation brings its spend back exactly. It is the repair path for
// spend recorded twice before copied history stopped re-counting its source.
func migrateV272toV273(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v272→v273 (cost ignored convs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS cost_ignored_convs (
		conv_id    TEXT PRIMARY KEY,
		ignored_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("migrate v272→v273 (cost ignored convs): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 273`); err != nil {
		return fmt.Errorf("migrate v272→v273 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v272→v273 (commit): %w", err)
	}
	return nil
}
