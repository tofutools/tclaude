package db

import (
	"database/sql"
	"fmt"
)

// migrateV92toV93 stands up the JOH-244 startup-choreography feature: staged
// spawn "waves" plus template-declared recurring "rhythms", and the cron
// role-filter primitive the rhythms materialize onto.
//
// Five additive schema changes, all following the v76–v92 convention
// (probe-guarded ADD COLUMN, CREATE TABLE IF NOT EXISTS, one transaction with
// the version bump — a half-applied run converges on re-run):
//
//   - group_template_agents.wave — the per-agent launch wave (INT, default 0).
//     A template whose every agent is wave 0 behaves EXACTLY like today: one
//     synchronous spawn pass. Higher waves spawn later, gated on the prior
//     wave settling. Rides the per-agent-scalar convention (v89 launch cols).
//   - group_templates.rhythms — the template's recurring-nudge declarations
//     (JSON: a list of {name, target_role, interval|cron, subject, body}).
//     Rides the same TEXT-JSON convention as work_pattern (v88) / process
//     (v92); '' / empty = no rhythms.
//   - group_templates.wave_max_wait — the per-template cap (seconds) on how
//     long a wave gate waits for the prior wave to go idle before the next
//     wave spawns anyway (INT, default 0 = "use the built-in default"). A
//     crashed lead can't wedge the force forever.
//   - agent_cron_jobs.target_role — a role filter on a group-target cron job
//     (TEXT, default '' = whole group). Resolved at fire time against the live
//     roster, so membership changes stay correct. A first-class cron primitive
//     (useful beyond task forces); rhythms materialize onto it.
//   - group_wave_choreography — the persisted, self-healing runtime state for
//     an in-flight staged deploy: one row per group with pending waves, keyed
//     by group_id. The whole choreography (remaining waves, the composed
//     spawn context, the gate cursor + deadline, the accumulated spawns) lives
//     in a JSON `state` blob so a daemon restart re-arms pending waves from it.
//     Cleaned up explicitly in DeleteAgentGroup's transaction (like the v92
//     process state), and dropped when the last wave lands.
func migrateV92toV93(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v92→v93: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// group_template_agents.wave — per-agent launch wave. Probe-guarded on the
	// table existing AND the column being absent (converge on re-run).
	haveAgents, err := txTableExists(tx, "group_template_agents")
	if err != nil {
		return fmt.Errorf("migrate v92→v93 (probe group_template_agents): %w", err)
	}
	if haveAgents {
		if err := addColumnIfMissing(tx, "group_template_agents", "wave",
			`ALTER TABLE group_template_agents ADD COLUMN wave INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v92→v93: %w", err)
		}
	}

	// group_templates.rhythms + wave_max_wait — the template-level additions.
	haveTemplates, err := txTableExists(tx, "group_templates")
	if err != nil {
		return fmt.Errorf("migrate v92→v93 (probe group_templates): %w", err)
	}
	if haveTemplates {
		if err := addColumnIfMissing(tx, "group_templates", "rhythms",
			`ALTER TABLE group_templates ADD COLUMN rhythms TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v92→v93: %w", err)
		}
		if err := addColumnIfMissing(tx, "group_templates", "wave_max_wait",
			`ALTER TABLE group_templates ADD COLUMN wave_max_wait INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v92→v93: %w", err)
		}
	}

	// agent_cron_jobs.target_role — the group-target role filter.
	haveCron, err := txTableExists(tx, "agent_cron_jobs")
	if err != nil {
		return fmt.Errorf("migrate v92→v93 (probe agent_cron_jobs): %w", err)
	}
	if haveCron {
		if err := addColumnIfMissing(tx, "agent_cron_jobs", "target_role",
			`ALTER TABLE agent_cron_jobs ADD COLUMN target_role TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v92→v93: %w", err)
		}
	}

	// group_wave_choreography — the persisted staged-deploy runtime state.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS group_wave_choreography (
			group_id   INTEGER PRIMARY KEY,
			group_name TEXT NOT NULL,
			state      TEXT NOT NULL DEFAULT '{}',
			updated_at TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("migrate v92→v93 (create group_wave_choreography): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 93`); err != nil {
		return fmt.Errorf("migrate v92→v93 (version): %w", err)
	}
	return tx.Commit()
}

// addColumnIfMissing runs alterSQL to add a column only when a
// pragma_table_info probe shows it absent, so a re-run after a partial apply
// is a clean no-op instead of a "duplicate column" error. The shared guard
// behind every additive column in this migration.
func addColumnIfMissing(tx *sql.Tx, table, column, alterSQL string) error {
	var have int
	if err := tx.QueryRow(
		fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?`, table), column,
	).Scan(&have); err != nil {
		return fmt.Errorf("probe %s.%s: %w", table, column, err)
	}
	if have > 0 {
		return nil
	}
	if _, err := tx.Exec(alterSQL); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}
