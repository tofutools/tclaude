package db

import (
	"context"
	"database/sql"
	"fmt"
)

// migrateV214toV215 adds agent state-condition sources and their durable
// per-rule/per-agent episode state. The existing event table keeps its
// historical name for wire/storage compatibility, but agent_pr_id becomes
// nullable and explicit agent/fact evidence is stored alongside PR evidence.
func migrateV214toV215(d *sql.DB) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate v214→v215 (trigger dwell): connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migrate v214→v215: disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate v214→v215: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE trigger_rules_v215 (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			row_version INTEGER NOT NULL DEFAULT 1,
			revision INTEGER NOT NULL DEFAULT 1,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			owner_agent TEXT NOT NULL DEFAULT '',
			operator_authored INTEGER NOT NULL DEFAULT 0 CHECK (operator_authored IN (0, 1)),
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'group')),
			group_id INTEGER REFERENCES agent_groups(id) ON DELETE CASCADE,
			source TEXT NOT NULL CHECK (source IN ('pr.opened','pr.updated','pr.merged','ci.failed','ci.succeeded','agent.idle','agent.awaiting_input')),
			author_is_agent INTEGER CHECK (author_is_agent IS NULL OR author_is_agent IN (0, 1)),
			draft_filter TEXT NOT NULL DEFAULT 'include' CHECK (draft_filter IN ('include', 'exclude', 'only')),
			debounce_seconds INTEGER NOT NULL DEFAULT 0 CHECK (debounce_seconds >= 0),
			cooldown_seconds INTEGER NOT NULL DEFAULT 0 CHECK (cooldown_seconds >= 0),
			for_seconds INTEGER NOT NULL DEFAULT 0 CHECK (for_seconds >= 0),
			actions_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK ((scope_kind = 'group') = (group_id IS NOT NULL))
		) STRICT;
		INSERT INTO trigger_rules_v215
			(id,name,row_version,revision,enabled,owner_agent,operator_authored,scope_kind,group_id,source,
			 author_is_agent,draft_filter,debounce_seconds,cooldown_seconds,actions_json,created_at,updated_at)
		SELECT id,name,row_version,revision,enabled,owner_agent,operator_authored,scope_kind,group_id,source,
		       author_is_agent,draft_filter,debounce_seconds,cooldown_seconds,actions_json,created_at,updated_at
		FROM trigger_rules;
		DROP TABLE trigger_rules;
		ALTER TABLE trigger_rules_v215 RENAME TO trigger_rules;
		CREATE INDEX idx_trigger_rules_enabled ON trigger_rules(enabled, source);
		CREATE INDEX idx_trigger_rules_group ON trigger_rules(group_id);

		CREATE TABLE trigger_pr_events_v215 (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_pr_id INTEGER REFERENCES agent_prs(id) ON DELETE CASCADE,
			origin_rule_id INTEGER REFERENCES trigger_rules(id) ON DELETE SET NULL,
			source TEXT NOT NULL CHECK (source IN ('pr.opened','pr.updated','pr.merged','ci.failed','ci.succeeded','agent.idle','agent.awaiting_input')),
			event_ref TEXT NOT NULL UNIQUE,
			pr_url TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL DEFAULT 0,
			pr_branch TEXT NOT NULL DEFAULT '',
			pr_author_agent TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			agent_harness TEXT NOT NULL DEFAULT '',
			fact_result TEXT NOT NULL DEFAULT '',
			fact_observed_at INTEGER,
			dwell_started_at INTEGER,
			draft INTEGER NOT NULL DEFAULT 0 CHECK (draft IN (0, 1)),
			group_ids_json TEXT NOT NULL DEFAULT '[]',
			previous_state TEXT NOT NULL DEFAULT '',
			current_state TEXT NOT NULL DEFAULT '',
			occurred_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processed', 'preexisting', 'interrupted')),
			processed_at INTEGER
		) STRICT;
		INSERT INTO trigger_pr_events_v215
			(id,agent_pr_id,source,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,draft,group_ids_json,
			 previous_state,current_state,occurred_at,updated_at,status,processed_at)
		SELECT id,agent_pr_id,source,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,draft,group_ids_json,
		       previous_state,current_state,occurred_at,updated_at,status,processed_at
		FROM trigger_pr_events;
		DROP TABLE trigger_pr_events;
		ALTER TABLE trigger_pr_events_v215 RENAME TO trigger_pr_events;
		CREATE INDEX idx_trigger_pr_events_pending ON trigger_pr_events(status, updated_at);
		CREATE INDEX idx_trigger_pr_events_pr_source ON trigger_pr_events(agent_pr_id, source, id);

		CREATE TABLE trigger_dwell_states (
			rule_id INTEGER NOT NULL REFERENCES trigger_rules(id) ON DELETE CASCADE,
			agent_id TEXT NOT NULL,
			rule_revision INTEGER NOT NULL,
			episode INTEGER NOT NULL DEFAULT 0 CHECK (episode >= 0),
			result TEXT NOT NULL CHECK (result IN ('true','false','unknown')),
			detail TEXT NOT NULL DEFAULT '',
			harness TEXT NOT NULL DEFAULT '',
			fact_observed_at INTEGER,
			true_since INTEGER,
			fired_at INTEGER,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY(rule_id, agent_id)
		) STRICT;
		CREATE INDEX idx_trigger_dwell_states_due ON trigger_dwell_states(result, true_since, fired_at);
	`); err != nil {
		return fmt.Errorf("migrate v214→v215: rebuild: %w", err)
	}
	if err := assertForeignKeyGraphIntact(ctx, tx, "migrate v214→v215"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version=215`); err != nil {
		return fmt.Errorf("migrate v214→v215: version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v214→v215: commit: %w", err)
	}
	return nil
}
