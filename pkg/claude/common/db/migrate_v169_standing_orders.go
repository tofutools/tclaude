package db

import (
	"database/sql"
	"fmt"
)

// migrateV168toV169 adds the standing-order tables (TCL-841): durable,
// operator-authored guidance that agentd delivers when a trigger matches,
// rather than on a wall clock.
//
// agent_standing_orders deliberately mirrors agent_cron_jobs' target
// vocabulary — owner_agent / target_kind / target_agent / group_id /
// target_role / enabled / disabled_reason / operator_authored — because a
// standing order IS a scheduled job whose clock is an event predicate. Sharing
// the column names (and therefore the group-retire, role-filter and
// attribution semantics they carry) is what keeps the two surfaces one concept
// for the operator instead of two that drift.
//
// agent_standing_order_deliveries is the evaluation ledger. It records why an
// order did or did not reach an agent, which is what makes `orders explain`
// answerable after the fact. It is intentionally NOT the inbox: an inline
// reminder that already reached the model must not also consume the
// recipient's unread backpressure budget.
func migrateV168toV169(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v168→v169 (standing orders): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_standing_orders (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			name              TEXT    NOT NULL DEFAULT '',
			revision          INTEGER NOT NULL DEFAULT 1,
			owner_agent       TEXT    NOT NULL DEFAULT '',
			target_kind       TEXT    NOT NULL DEFAULT 'conv',
			target_agent      TEXT    NOT NULL DEFAULT '',
			group_id          INTEGER NOT NULL DEFAULT 0,
			target_role       TEXT    NOT NULL DEFAULT '',
			summary           TEXT    NOT NULL DEFAULT '',
			trigger_event     TEXT    NOT NULL DEFAULT '',
			trigger_sources   TEXT    NOT NULL DEFAULT '',
			timing            TEXT    NOT NULL DEFAULT 'next-turn',
			cadence           TEXT    NOT NULL DEFAULT 'always',
			enabled           INTEGER NOT NULL DEFAULT 1,
			disabled_reason   TEXT    NOT NULL DEFAULT '',
			operator_authored INTEGER NOT NULL DEFAULT 0,
			created_at        TEXT    NOT NULL,
			updated_at        TEXT    NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_standing_orders_owner
			ON agent_standing_orders(owner_agent);
		CREATE INDEX IF NOT EXISTS idx_agent_standing_orders_group
			ON agent_standing_orders(group_id);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_standing_orders_name
			ON agent_standing_orders(name);

		CREATE TABLE IF NOT EXISTS agent_standing_order_deliveries (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id       INTEGER NOT NULL,
			order_revision INTEGER NOT NULL DEFAULT 0,
			target_conv    TEXT    NOT NULL DEFAULT '',
			epoch          TEXT    NOT NULL DEFAULT '',
			outcome        TEXT    NOT NULL DEFAULT '',
			transport      TEXT    NOT NULL DEFAULT '',
			harness        TEXT    NOT NULL DEFAULT '',
			detail         TEXT    NOT NULL DEFAULT '',
			created_at     TEXT    NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_standing_order_deliveries_order
			ON agent_standing_order_deliveries(order_id, created_at);
		CREATE INDEX IF NOT EXISTS idx_agent_standing_order_deliveries_epoch
			ON agent_standing_order_deliveries(order_id, order_revision, target_conv, epoch);
	`); err != nil {
		return fmt.Errorf("migrate v168→v169 (standing orders): create: %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 169`); err != nil {
		return fmt.Errorf("migrate v168→v169 (standing orders): set version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v168→v169 (standing orders): commit: %w", err)
	}
	return nil
}
