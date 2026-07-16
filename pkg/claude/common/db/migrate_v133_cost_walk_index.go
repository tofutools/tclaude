package db

import (
	"database/sql"
	"fmt"
)

// migrateV132toV133 adds the ordering index used by AllCostDailyRows. The
// dashboard reads the complete cost history every two seconds to recover
// per-day deltas across carry-forward and fresh-counter resumes. Its canonical
// ordering starts with a fallback expression, so the existing day-only index
// cannot help and SQLite otherwise rebuilds a temporary B-tree for every poll.
func migrateV132toV133(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v132→v133: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_session_cost_daily_walk
		ON session_cost_daily(
			COALESCE(NULLIF(conv_id, ''), session_id), day, updated_at, session_id
		)`); err != nil {
		return fmt.Errorf("migrate v132→v133 (create cost walk index): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 133`); err != nil {
		return fmt.Errorf("migrate v132→v133 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v132→v133 (commit): %w", err)
	}
	return nil
}
