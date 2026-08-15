package db

import (
	"database/sql"
	"fmt"
)

// migrateV211toV212 adds the first tclaude-level trigger slice. PR events are
// durable before agentd evaluates them, and the firing/action tables are an
// append-only explanation ledger. Existing presented PRs are inserted as
// preexisting observations so installing the feature never replays history.
func migrateV211toV212(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v211→v212 (triggers): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS trigger_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			row_version INTEGER NOT NULL DEFAULT 1,
			revision INTEGER NOT NULL DEFAULT 1,
			enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
			owner_agent TEXT NOT NULL DEFAULT '',
			operator_authored INTEGER NOT NULL DEFAULT 0 CHECK (operator_authored IN (0, 1)),
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'group')),
			group_id INTEGER REFERENCES agent_groups(id) ON DELETE CASCADE,
			source TEXT NOT NULL CHECK (source = 'pr.opened'),
			author_is_agent INTEGER CHECK (author_is_agent IS NULL OR author_is_agent IN (0, 1)),
			draft_filter TEXT NOT NULL DEFAULT 'include' CHECK (draft_filter IN ('include', 'exclude', 'only')),
			debounce_seconds INTEGER NOT NULL DEFAULT 0 CHECK (debounce_seconds >= 0),
			cooldown_seconds INTEGER NOT NULL DEFAULT 0 CHECK (cooldown_seconds >= 0),
			actions_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK ((scope_kind = 'group') = (group_id IS NOT NULL))
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_trigger_rules_enabled ON trigger_rules(enabled, source);
		CREATE INDEX IF NOT EXISTS idx_trigger_rules_group ON trigger_rules(group_id);

		CREATE TABLE IF NOT EXISTS trigger_pr_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_pr_id INTEGER NOT NULL UNIQUE REFERENCES agent_prs(id) ON DELETE CASCADE,
			event_ref TEXT NOT NULL UNIQUE,
			pr_url TEXT NOT NULL,
			pr_number INTEGER NOT NULL DEFAULT 0,
			pr_branch TEXT NOT NULL DEFAULT '',
			pr_author_agent TEXT NOT NULL,
			draft INTEGER NOT NULL DEFAULT 0 CHECK (draft IN (0, 1)),
			group_ids_json TEXT NOT NULL DEFAULT '[]',
			occurred_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processed', 'preexisting', 'interrupted')),
			processed_at INTEGER
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_trigger_pr_events_pending ON trigger_pr_events(status, updated_at);

		CREATE TABLE IF NOT EXISTS trigger_firings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id INTEGER REFERENCES trigger_rules(id) ON DELETE SET NULL,
			rule_revision INTEGER NOT NULL,
			event_id INTEGER NOT NULL REFERENCES trigger_pr_events(id) ON DELETE CASCADE,
			event_ref TEXT NOT NULL,
			outcome TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			started_at INTEGER NOT NULL,
			finished_at INTEGER,
			UNIQUE(rule_id, rule_revision, event_id)
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_trigger_firings_rule ON trigger_firings(rule_id, started_at DESC);

		CREATE TABLE IF NOT EXISTS trigger_action_outcomes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			firing_id INTEGER NOT NULL REFERENCES trigger_firings(id) ON DELETE CASCADE,
			action_index INTEGER NOT NULL,
			action_type TEXT NOT NULL,
			outcome TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			spawned_agent TEXT NOT NULL DEFAULT '',
			message_id INTEGER,
			created_at INTEGER NOT NULL,
			UNIQUE(firing_id, action_index)
		) STRICT;

		CREATE TABLE IF NOT EXISTS trigger_workers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id INTEGER REFERENCES trigger_rules(id) ON DELETE SET NULL,
			firing_id INTEGER NOT NULL REFERENCES trigger_firings(id) ON DELETE CASCADE,
			action_index INTEGER NOT NULL,
			agent_id TEXT NOT NULL UNIQUE,
			conv_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved', 'pending', 'live', 'failed', 'exited', 'deadline_exceeded')),
			pending_label TEXT NOT NULL DEFAULT '',
			deadline_at INTEGER,
			created_at INTEGER NOT NULL,
			completed_at INTEGER,
			detail TEXT NOT NULL DEFAULT ''
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_trigger_workers_rule_live ON trigger_workers(rule_id, state);

		CREATE TABLE IF NOT EXISTS daemon_spawn_history (
			principal TEXT NOT NULL,
			spawned_at INTEGER NOT NULL
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_daemon_spawn_history_principal
			ON daemon_spawn_history(principal, spawned_at);
	`); err != nil {
		return fmt.Errorf("migrate v211→v212 (triggers): apply: %w", err)
	}
	havePRs, err := txTableExists(tx, "agent_prs")
	if err != nil {
		return fmt.Errorf("migrate v211→v212 (triggers): probe agent_prs: %w", err)
	}
	if havePRs {
		haveMembers, probeErr := txTableExists(tx, "agent_group_members")
		if probeErr != nil {
			return fmt.Errorf("migrate v211→v212 (triggers): probe agent_group_members: %w", probeErr)
		}
		groupsExpr := `'[]'`
		if haveMembers {
			groupsExpr = `COALESCE((SELECT json_group_array(m.group_id)
				FROM agent_group_members m WHERE m.agent_id = p.agent_id), '[]')`
		}
		if _, err = tx.Exec(`
			INSERT OR IGNORE INTO trigger_pr_events
				(agent_pr_id, event_ref, pr_url, pr_number, pr_author_agent, draft,
				 group_ids_json, occurred_at, updated_at, status, processed_at)
			SELECT p.id, 'pr.opened:' || p.agent_id || ':' || p.pr_url, p.pr_url, 0,
			       p.agent_id, CASE WHEN lower(trim(p.state)) = 'draft' THEN 1 ELSE 0 END,
			       ` + groupsExpr + `, p.created_at, p.updated_at, 'preexisting', p.updated_at
			FROM agent_prs p;
		`); err != nil {
			return fmt.Errorf("migrate v211→v212 (triggers): seed existing PRs: %w", err)
		}
	}
	if _, err = tx.Exec(`UPDATE schema_version SET version = 212`); err != nil {
		return fmt.Errorf("migrate v211→v212 (triggers): version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate v211→v212 (triggers): commit: %w", err)
	}
	return nil
}
