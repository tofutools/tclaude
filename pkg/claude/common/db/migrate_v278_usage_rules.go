package db

import (
	"database/sql"
	"fmt"
)

// migrateV277toV278 adds usage limits and usage profiles.
//
//   - usage_profiles: named bundles of per-agent rules (JSON array of
//     usagerules.Rule). Each agent they apply to gets its own counter.
//   - usage_profile_assignments: which profiles apply at which layer —
//     global (empty scope_id), group (scope_id = group id), or agent (stable
//     agent id). The nearest non-empty layer wins; every profile on that layer
//     applies.
//   - usage_limits: one rule over an aggregate (global or one group), with a
//     single shared counter.
//   - usage_agent_overrides / usage_budget_extensions: per-agent operator
//     adjustments (exempt, snooze, extra budget for one rule).
//   - usage_rule_firings: one row per fired step occurrence. The primary key is
//     the dedupe: a step fires once per (rule, subject, period span + budget).
func migrateV277toV278(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v277→v278 (usage rules): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS usage_profiles (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
			descr      TEXT NOT NULL DEFAULT '',
			rules      TEXT NOT NULL DEFAULT '[]',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS usage_profile_assignments (
			scope      TEXT NOT NULL CHECK (scope IN ('global', 'group', 'agent')),
			scope_id   TEXT NOT NULL DEFAULT '',
			profile_id INTEGER NOT NULL REFERENCES usage_profiles(id) ON DELETE CASCADE,
			source     TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			PRIMARY KEY (scope, scope_id, profile_id)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_usage_profile_assignments_profile
			ON usage_profile_assignments(profile_id)`,
		`CREATE TRIGGER IF NOT EXISTS usage_profile_assignments_group_gone
			AFTER DELETE ON agent_groups BEGIN
				DELETE FROM usage_profile_assignments
				WHERE scope = 'group' AND scope_id = CAST(OLD.id AS TEXT);
			END`,
		`CREATE TRIGGER IF NOT EXISTS usage_profile_assignments_agent_gone
			AFTER DELETE ON agents BEGIN
				DELETE FROM usage_profile_assignments
				WHERE scope = 'agent' AND scope_id = OLD.agent_id;
			END`,
		`CREATE TABLE IF NOT EXISTS usage_limits (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			scope      TEXT NOT NULL CHECK (scope IN ('global', 'group')),
			group_id   INTEGER REFERENCES agent_groups(id) ON DELETE CASCADE,
			rule       TEXT NOT NULL,
			enabled    INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK ((scope = 'global') = (group_id IS NULL))
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_usage_limits_group ON usage_limits(group_id)`,
		`CREATE TABLE IF NOT EXISTS usage_agent_overrides (
			agent_id     TEXT PRIMARY KEY REFERENCES agents(agent_id) ON DELETE CASCADE,
			exempt       INTEGER NOT NULL DEFAULT 0,
			snooze_until INTEGER NOT NULL DEFAULT 0,
			updated_at   INTEGER NOT NULL
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS usage_budget_extensions (
			agent_id   TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			rule_key   TEXT NOT NULL,
			extra      REAL NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (agent_id, rule_key)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS usage_rule_firings (
			rule_key   TEXT NOT NULL,
			subject    TEXT NOT NULL,
			period_key TEXT NOT NULL,
			step_key   TEXT NOT NULL,
			action     TEXT NOT NULL,
			rule_name  TEXT NOT NULL DEFAULT '',
			value      REAL NOT NULL,
			budget     REAL NOT NULL,
			dry_run    INTEGER NOT NULL DEFAULT 0,
			fired_at   INTEGER NOT NULL,
			PRIMARY KEY (rule_key, subject, period_key, step_key)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_usage_rule_firings_fired ON usage_rule_firings(fired_at)`,
		`UPDATE schema_version SET version = 278`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v277→v278 (usage rules): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v277→v278 (commit): %w", err)
	}
	return nil
}
