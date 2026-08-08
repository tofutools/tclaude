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
	if _, err := tx.Exec(`UPDATE schema_version SET version = 195`); err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v194→v195 (reset Copilot usage snapshots): commit: %w", err)
	}
	return nil
}
