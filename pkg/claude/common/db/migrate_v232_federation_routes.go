package db

import (
	"database/sql"
	"fmt"
)

// migrateV231toV232 adds the federation route markers.
//
// A remote route is consumed through a local "mirror" route: an
// agent_routes row owned by the consuming agent whose broker publisher is
// agentd's federation proxy rather than a sandbox helper.
// federation_route_mirrors marks those rows. On the publishing side, agentd
// attaches to the real route as a consumer through a "proxy" lease held in
// the publisher's name; federation_route_proxies marks those leases. Marked
// rows are hidden from the route and lease listings (which launch helpers
// sync from) and refused on the helper channel, so no sandbox helper ever
// attaches to them.
func migrateV231toV232(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v231→v232 (federation routes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(federationRoutesSchema); err != nil {
		return fmt.Errorf("migrate v231→v232 (federation routes): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 232`); err != nil {
		return fmt.Errorf("migrate v231→v232 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v231→v232 (commit): %w", err)
	}
	return nil
}

const federationRoutesSchema = `
CREATE TABLE IF NOT EXISTS federation_route_mirrors (
	route_id      TEXT PRIMARY KEY REFERENCES agent_routes(id) ON DELETE CASCADE,
	peer          TEXT NOT NULL,
	remote_route  TEXT NOT NULL,
	remote_label  TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS federation_route_proxies (
	lease_id      TEXT PRIMARY KEY REFERENCES agent_route_leases(id) ON DELETE CASCADE,
	peer          TEXT NOT NULL,
	route_id      TEXT NOT NULL,
	created_at    INTEGER NOT NULL
) STRICT;
`
