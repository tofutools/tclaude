package db

import (
	"database/sql"
	"fmt"
)

// migrateV227toV228 retains provider-native quota units beside percentages.
// Copilot exposes exact AIC usage and entitlement values; preserving them per
// observation lets the dashboard render history correctly if a plan limit
// changes instead of multiplying old percentages by today's allowance.
func migrateV227toV228(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v227→v228 (subscription usage units): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, column := range []struct{ name, sql string }{
		{"used_units", `ALTER TABLE subscription_usage_windows ADD COLUMN used_units REAL NOT NULL DEFAULT 0`},
		{"limit_units", `ALTER TABLE subscription_usage_windows ADD COLUMN limit_units REAL NOT NULL DEFAULT 0`},
	} {
		if err := addColumnIfMissing(tx, "subscription_usage_windows", column.name, column.sql); err != nil {
			return fmt.Errorf("migrate v227→v228 (subscription usage units): %s: %w", column.name, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 228`); err != nil {
		return fmt.Errorf("migrate v227→v228 (subscription usage units): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v227→v228 (subscription usage units): commit: %w", err)
	}
	return nil
}
