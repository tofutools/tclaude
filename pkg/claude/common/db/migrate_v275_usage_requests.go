package db

import (
	"database/sql"
	"fmt"
)

// migrateV274toV275 lets an operator access request ask for usage on a
// peer's model gateway: the gateway and dollar amount asked for, and what
// the approval granted (the approver picks the gateway and may lower the
// amount).
func migrateV274toV275(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v274→v275 (usage requests): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE federation_peer_access_requests ADD COLUMN http_proxy TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE federation_peer_access_requests ADD COLUMN grant_http_proxy TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE federation_peer_access_requests ADD COLUMN max_usd REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE federation_peer_access_requests ADD COLUMN grant_max_usd REAL NOT NULL DEFAULT 0`,
		`UPDATE schema_version SET version = 275`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v274→v275 (usage requests): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v274→v275 (commit): %w", err)
	}
	return nil
}
