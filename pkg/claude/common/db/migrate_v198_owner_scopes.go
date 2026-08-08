package db

import (
	"database/sql"
	"fmt"
)

// migrateV197toV198 adds the per-group owner-scope map: the narrowing an
// operator may put on the STRUCTURAL owner-implied bypass for one group.
//
// Empty means "today's unrestricted bypass", so every existing group keeps its
// exact pre-Phase-6 authorization behaviour. The value is the same canonical
// JSON shape the grant tiers already store, one level deeper — slug → scope —
// and it rides on the group row rather than a side table because it is a small
// document edited with the group's own lifecycle and READ on nearly every
// owner-bypass check, where a join per check would be pure cost.
//
// group_templates carries the same column so an instantiated group is born
// with the narrowing its blueprint declares, instead of a wide-open bypass
// that has to be narrowed by hand after every deploy.
//
// The byte bound mirrors agent_permissions.scope_json (v196) and the agentd
// writer's own validator.
func migrateV197toV198(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v197→v198 (owner scopes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"agent_groups", "group_templates"} {
		exists, err := txTableExists(tx, table)
		if err != nil {
			return fmt.Errorf("migrate v197→v198 (inspect %s table): %w", table, err)
		}
		if !exists {
			continue
		}
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'owner_scopes_json'`, table).Scan(&present); err != nil {
			return fmt.Errorf("migrate v197→v198 (inspect %s): %w", table, err)
		}
		if present != 0 {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN owner_scopes_json TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(owner_scopes_json AS BLOB)) BETWEEN 0 AND 262144)`, table)
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v197→v198 (add %s.owner_scopes_json): %w", table, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 198`); err != nil {
		return fmt.Errorf("migrate v197→v198 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v197→v198 (commit): %w", err)
	}
	return nil
}
