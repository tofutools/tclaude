package db

import (
	"database/sql"
	"fmt"
)

// migrateV194toV195 discards the derived Copilot usage fold so the poller can
// replay it with the corrected per-call nano-AIU semantics.
//
// The v188-v194 fold treated each assistant_usage_events.total_nano_aiu as a
// running session total and retained only the newest value. Copilot 1.0.77's
// shipped schema and measured 1.0.78 stores both define it as one call's cost.
// Keeping an old cursor while changing the fold would permanently miss every
// already-consumed call, so the whole derived row must restart at event zero.
// The new fold_version column deliberately has no default. A v194 daemon that
// stayed alive across the migration does not name that column, so SQLite
// rejects both its INSERT and INSERT...ON CONFLICT shapes at the NOT NULL
// constraint instead of letting it recreate or overwrite a v1 cursor with the
// old last-row-wins fold. The new poller also validates the marker before it
// trusts any stored cursor.
//
// No Copilot-owned data is touched; live sessions rebuild this cache from the
// CLI's read-only session-store.db on the next sweep. The snapshot's nano-AIU
// is not published into sessions or any dashboard/API result today, while the
// durable event follower retains Copilot's authoritative checkpoint
// independently. Thus even if Copilot has pruned an old store row, this reset
// can only replace a known-wrong derived cache value; it cannot regress a
// user-visible or durable session figure.
func migrateV194toV195(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM copilot_usage_snapshots`); err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): clear: %w", err)
	}
	var haveFoldVersion int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('copilot_usage_snapshots')
		WHERE name = 'fold_version'`).Scan(&haveFoldVersion); err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): probe: %w", err)
	}
	if haveFoldVersion == 0 {
		if _, err := tx.Exec(`ALTER TABLE copilot_usage_snapshots
			ADD COLUMN fold_version INTEGER NOT NULL`); err != nil {
			return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): add fold version: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 195`); err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): commit: %w", err)
	}
	return nil
}
