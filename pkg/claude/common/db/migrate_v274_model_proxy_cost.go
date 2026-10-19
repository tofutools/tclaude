package db

import (
	"database/sql"
	"fmt"
)

// migrateV273toV274 prices model gateway requests for usage sharing: each
// request records its dollar charge in micro-USD (reserved, then settled like
// charged_tokens) and whether the gateway knew the model's price. The index
// serves a share's spend since its grant began (proxy, peer, started_at).
func migrateV273toV274(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v273→v274 (model proxy cost): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE model_proxy_requests ADD COLUMN charged_micro_usd INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE model_proxy_requests ADD COLUMN priced INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS model_proxy_share ON model_proxy_requests(peer,proxy,started_at)`,
		`UPDATE schema_version SET version = 274`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v273→v274 (model proxy cost): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v273→v274 (commit): %w", err)
	}
	return nil
}
