package db

import (
	"database/sql"
	"fmt"
)

// v83OwnerPermColumns are the birth-time access-control columns the spawn
// dialog's "Group owner" checkbox + permission editor add, spread
// across two tables:
//
//   - pending_spawns — a dashboard spawn whose conv-id has not materialised yet
//     carries the requested owner flag + per-slug overrides so the pending-spawn
//     sweeper can apply them when it back-fills the enrollment minutes later (a
//     gated Codex). is_owner is a plain 0/1 flag (the spawn either is or isn't an
//     owner); permission_overrides is a JSON object mapping slug → "grant" |
//     "deny" ("" = none).
//   - spawn_profiles — a saved profile can carry the same intent so it pre-fills
//     the spawn dialog. is_owner is NULLABLE here (tri-state, like the other
//     profile toggles: NULL = unset → leave the dialog default); permission_overrides
//     is the same JSON-object TEXT ("" = unset).
//
// The inline CC/Codex spawn paths apply pending_spawns' values straight from
// spawnParams and never read these columns back; they exist so the durable rows
// stay complete intents.
var v83OwnerPermColumns = []struct {
	table string
	col   string
	decl  string
}{
	{"pending_spawns", "is_owner", "INTEGER NOT NULL DEFAULT 0"},
	{"pending_spawns", "permission_overrides", "TEXT NOT NULL DEFAULT ''"},
	{"spawn_profiles", "is_owner", "INTEGER"},
	{"spawn_profiles", "permission_overrides", "TEXT NOT NULL DEFAULT ''"},
}

// migrateV82toV83 adds the v83OwnerPermColumns to pending_spawns + spawn_profiles.
//
// Additive + idempotent (the v76–v81 convention): each table is probed so a
// partial-schema heal DB missing one is a clean skip; each ADD COLUMN is guarded
// by a pragma_table_info probe so a half-applied run converges on re-run instead
// of wedging on "duplicate column". The NOT NULL DEFAULT columns back-fill
// existing rows to "no owner / no overrides" with no data pass; the nullable
// spawn_profiles.is_owner back-fills to NULL (unset). The whole thing rides one
// transaction with the version bump; no VACUUM snapshot — nothing is dropped or
// rewritten.
func migrateV82toV83(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v82→v83: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, spec := range v83OwnerPermColumns {
		haveTable, err := txTableExists(tx, spec.table)
		if err != nil {
			return fmt.Errorf("migrate v82→v83 (probe %s): %w", spec.table, err)
		}
		if !haveTable {
			continue // partial-schema heal DB without this table — skip
		}
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, spec.table, spec.col,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v82→v83 (probe %s.%s): %w", spec.table, spec.col, err)
		}
		if have == 0 {
			// Table / column / decl come from the hardcoded spec list above,
			// never user input.
			if _, err := tx.Exec(
				`ALTER TABLE ` + spec.table + ` ADD COLUMN ` + spec.col + ` ` + spec.decl,
			); err != nil {
				return fmt.Errorf("migrate v82→v83 (add %s.%s): %w", spec.table, spec.col, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 83`); err != nil {
		return fmt.Errorf("migrate v82→v83 (version): %w", err)
	}
	return tx.Commit()
}
