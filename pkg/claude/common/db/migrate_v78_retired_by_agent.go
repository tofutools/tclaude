package db

import (
	"database/sql"
	"fmt"
)

// migrateV77toV78 adds agents.retired_by_agent — the durable agent_id companion
// to agents.retired_by (JOH-306). retired_by keeps its raw audit value: a
// conv-id when an agent performed the retire, or a literal ("human",
// "system:export-clone"). The new column carries the rotation-immune stable
// agent_id of that retirer, so the dashboard's retired "by" can show a real
// agent name / agent_id instead of a meaningless, rotation-fragile conv-id.
//
// Same shape and the SAME backfill derivation as the v77 companion columns
// (backfillAgentColSQL): resolve retired_by → owning actor via
// agent_conversations, leaving it empty for a literal or an unmapped conv. It is an
// "author" column ("who retired it"): the retirer's conv is enrolled by the
// time the retire is recorded, so derive-at-write (db.RetireAgentByID) covers
// new rows and this migration backfills the existing ones — the two agree by
// construction.
//
// Additive + idempotent (the v76 / v77 convention): the agents-table probe
// makes a partial-schema heal DB a clean skip; the pragma_table_info guard lets
// a half-applied run converge on re-run instead of wedging on "duplicate
// column"; the backfill UPDATE recomputes the same join (naturally idempotent)
// and is skipped when the agent_conversations resolution spine or the
// retired_by source column is absent. The whole pass runs in one transaction,
// so a failure rolls back and the version stays at 77. No VACUUM snapshot:
// nothing is dropped or rewritten.
func migrateV77toV78(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v77→v78: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveAgents, err := txTableExists(tx, "agents")
	if err != nil {
		return fmt.Errorf("migrate v77→v78 (probe agents): %w", err)
	}
	if haveAgents {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'retired_by_agent'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v77→v78 (probe agents.retired_by_agent): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agents ADD COLUMN retired_by_agent TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v77→v78 (add agents.retired_by_agent): %w", err)
			}
		}

		// Backfill only when the resolution spine (agent_conversations) and the
		// source column (retired_by) are both present — a partial-schema heal DB
		// may lack either, in which case the ADD COLUMN above still stood the
		// companion up empty. A literal retired_by resolves to no actor row, so
		// COALESCE leaves it empty; an unresolvable conv likewise. The migration
		// never fails on an unresolvable retirer.
		haveConvs, err := txTableExists(tx, "agent_conversations")
		if err != nil {
			return fmt.Errorf("migrate v77→v78 (probe agent_conversations): %w", err)
		}
		var haveRetiredBy int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'retired_by'`,
		).Scan(&haveRetiredBy); err != nil {
			return fmt.Errorf("migrate v77→v78 (probe agents.retired_by): %w", err)
		}
		if haveConvs && haveRetiredBy > 0 {
			if _, err := tx.Exec(backfillAgentColSQL("agents", "retired_by_agent", []string{"retired_by"}, false)); err != nil {
				return fmt.Errorf("migrate v77→v78 (backfill agents.retired_by_agent): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 78`); err != nil {
		return fmt.Errorf("migrate v77→v78 (version): %w", err)
	}
	return tx.Commit()
}
