package db

import (
	"database/sql"
	"fmt"
)

// migrateV225toV226 adds the peer-messaging posture columns (TCL-812), the
// exact twin of the v139 auto-memory pair:
//
//   - spawn_profiles.peer_messaging — nullable INTEGER, the profile's tri-state
//     opt-in (NULL = unset, 0 = peer messaging off, 1 = peer messaging on).
//   - sessions.peer_messaging — NOT NULL INTEGER, the resolved posture the
//     launch actually ran with, so a resume reproduces it instead of silently
//     falling back to the harness default.
//
// Claude Code ships its own cross-session messaging mesh — a second, unmanaged
// coordination channel with none of the group, permission or audit properties
// tclaude's own agent messaging has — so tclaude resolves an unset profile to
// OFF and injects the refusal at spawn (see harness/peer_messaging.go). The
// sessions column's 0 default therefore reads as "peer messaging off" for
// legacy rows, which is exactly the posture a resumed legacy session should
// get: it is what tclaude will launch every Claude Code session with from here
// on, so a resumed row converges on the new default rather than preserving a
// posture nobody chose.
//
// Additive, probe-guarded ADD COLUMNs in one transaction (the migrateV110toV111
// convention) so a half-applied run converges on re-run.
func migrateV225toV226(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v225→v226 (peer messaging): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	adds := []struct {
		table  string
		column string
		decl   string
	}{
		{"spawn_profiles", "peer_messaging", "INTEGER"},
		{"sessions", "peer_messaging", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, add := range adds {
		var haveTable int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, add.table,
		).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v225→v226 (probe %s): %w", add.table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, add.table, add.column,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v225→v226 (probe %s.%s): %w", add.table, add.column, err)
		}
		if haveColumn > 0 {
			continue
		}
		if _, err := tx.Exec(
			`ALTER TABLE ` + add.table + ` ADD COLUMN ` + add.column + ` ` + add.decl,
		); err != nil {
			return fmt.Errorf("migrate v225→v226 (add %s.%s): %w", add.table, add.column, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 226`); err != nil {
		return fmt.Errorf("migrate v225→v226 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v225→v226 (commit): %w", err)
	}
	return nil
}
