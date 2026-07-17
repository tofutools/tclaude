package db

import (
	"database/sql"
	"fmt"
)

// migrateV133toV134 adds provider-neutral subscription-usage history. The
// existing usage_cache and codex_usage_cache tables intentionally retain only
// the newest account-wide reading; they cannot drive a graph or consumption
// forecast. Samples are therefore stored separately at a bounded cadence,
// with one child row per provider-defined rate-limit window.
//
// sampled_at is the start of a UTC sampling bucket. Together with provider it
// makes concurrent statusline/hook writers converge on one snapshot per
// interval. Each child carries its own observed_at/source because providers
// may omit one window while reporting another; the newest genuine reading for
// each window wins without turning display-only carry-forward into history.
// Window names and durations are data rather than columns, allowing
// weekly-only accounts and future providers/windows without another schema
// migration.
func migrateV133toV134(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v133→v134: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS subscription_usage_samples (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			provider   TEXT NOT NULL,
			sampled_at TEXT NOT NULL,
			UNIQUE(provider, sampled_at)
		);
		CREATE INDEX IF NOT EXISTS idx_subscription_usage_samples_sampled_at
			ON subscription_usage_samples(sampled_at);

		CREATE TABLE IF NOT EXISTS subscription_usage_windows (
			sample_id        INTEGER NOT NULL REFERENCES subscription_usage_samples(id) ON DELETE CASCADE,
			window_name      TEXT NOT NULL,
			duration_seconds INTEGER NOT NULL DEFAULT 0,
			used_percent     REAL NOT NULL,
			resets_at        TEXT NOT NULL DEFAULT '',
			observed_at      TEXT NOT NULL,
			source           TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(sample_id, window_name)
		);
	`); err != nil {
		return fmt.Errorf("migrate v133→v134 (create subscription usage history): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 134`); err != nil {
		return fmt.Errorf("migrate v133→v134 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v133→v134 (commit): %w", err)
	}
	return nil
}
