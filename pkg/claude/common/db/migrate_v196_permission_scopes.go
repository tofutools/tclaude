package db

import (
	"database/sql"
	"fmt"
)

// migrateV195toV196 adds the optional, canonical JSON scope carried by each
// grant tier. Empty means unscoped and therefore preserves every existing
// authorization decision. The byte bound mirrors the v147 JSON-column style
// and the agentd grant-path validator.
func migrateV195toV196(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v195→v196 (permission scopes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"agent_permissions", "agent_group_permissions", "agent_sudo_grants"} {
		exists, err := txTableExists(tx, table)
		if err != nil {
			return fmt.Errorf("migrate v195→v196 (inspect %s table): %w", table, err)
		}
		if !exists {
			continue
		}
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'scope_json'`, table).Scan(&present); err != nil {
			return fmt.Errorf("migrate v195→v196 (inspect %s): %w", table, err)
		}
		if present != 0 {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN scope_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(scope_json AS BLOB)) BETWEEN 0 AND 262144)`, table)
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v195→v196 (add %s.scope_json): %w", table, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 196`); err != nil {
		return fmt.Errorf("migrate v195→v196 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v195→v196 (commit): %w", err)
	}
	return nil
}
