package db

import (
	"database/sql"
	"fmt"
)

// migrateV182toV183 installs the durable authority-side route registry. The
// data plane is intentionally absent: route and lease rows are only the
// authenticated lifecycle contract, while adapters and stream forwarding are
// later milestones.
func migrateV182toV183(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v182→v183 (group routes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	haveGroups, err := txTableExists(tx, "agent_groups")
	if err != nil {
		return fmt.Errorf("migrate v182→v183 (group routes): probe agent_groups: %w", err)
	}
	// Historical migration-heal fixtures can legitimately reach head without
	// the optional group subsystem (the v55/v60 half-applied contracts). There
	// is no route registry to install without its parent table, so advance the
	// version while preserving the fixture's converged shape. A real database
	// with agent_groups still takes the strict route-schema path below; errors
	// from that path remain fatal rather than being masked.
	if !haveGroups {
		if _, err := tx.Exec(`UPDATE schema_version SET version = 183`); err != nil {
			return fmt.Errorf("migrate v182→v183 (group routes, no-op): %w", err)
		}
		return tx.Commit()
	}
	var haveRouteGeneration int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'route_generation'`).Scan(&haveRouteGeneration); err != nil {
		return fmt.Errorf("migrate v182→v183 (group routes): probe route generation: %w", err)
	}
	if haveRouteGeneration == 0 {
		if _, err := tx.Exec(`ALTER TABLE agent_groups ADD COLUMN route_generation INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v182→v183 (group routes): add route generation: %w", err)
		}
	}
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_routes (
			id TEXT PRIMARY KEY,
			group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			publisher_agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			publisher_conv_id TEXT NOT NULL DEFAULT '',
			publisher_launch_generation TEXT NOT NULL,
			group_generation INTEGER NOT NULL,
			name TEXT NOT NULL,
			transport TEXT NOT NULL,
			target TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('ready', 'draining', 'withdrawn', 'publisher-lost')),
			created_at INTEGER NOT NULL,
			withdrawn_at INTEGER,
			withdraw_reason TEXT NOT NULL DEFAULT '',
			UNIQUE(group_id, publisher_agent_id, name)
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_agent_routes_group_state ON agent_routes(group_id, state);
		CREATE INDEX IF NOT EXISTS idx_agent_routes_publisher ON agent_routes(publisher_agent_id, state);

		CREATE TABLE IF NOT EXISTS agent_route_leases (
			id TEXT PRIMARY KEY,
			route_id TEXT NOT NULL REFERENCES agent_routes(id) ON DELETE CASCADE,
			consumer_agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			consumer_conv_id TEXT NOT NULL DEFAULT '',
			consumer_launch_generation TEXT NOT NULL,
			group_generation INTEGER NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('open', 'closed')),
			opened_at INTEGER NOT NULL,
			closed_at INTEGER
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_agent_route_leases_route_state ON agent_route_leases(route_id, state);
		CREATE INDEX IF NOT EXISTS idx_agent_route_leases_consumer ON agent_route_leases(consumer_agent_id, state);

		CREATE TABLE IF NOT EXISTS agent_route_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			action TEXT NOT NULL,
			result TEXT NOT NULL,
			group_id INTEGER,
			route_id TEXT NOT NULL DEFAULT '',
			lease_id TEXT NOT NULL DEFAULT '',
			actor_agent_id TEXT NOT NULL DEFAULT '',
			actor_conv_id TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT ''
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_agent_route_audit_route ON agent_route_audit(route_id, at);

		UPDATE schema_version SET version = 183;
	`); err != nil {
		return fmt.Errorf("migrate v182→v183 (group routes): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v182→v183 (group routes): commit: %w", err)
	}
	return nil
}
