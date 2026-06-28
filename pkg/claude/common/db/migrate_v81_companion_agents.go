package db

import (
	"database/sql"
	"fmt"
)

// v81CompanionAgentColumns are the agent_id companions JOH-281/284 left out:
// the conv-keyed columns that, unlike the v77 set, had NO agent reference at
// all, so their sender/routing attribution was reconstructed by re-resolving
// conv→agent at read time — which breaks once the referenced generation is
// pruned/rotated (JOH-321 F2).
//
// Each entry adds one TEXT column to `table`, backfilled to the owning actor of
// the row's conv reference via agent_conversations (COALESCE over convCols,
// first non-empty wins), falling back to ” for a non-actor / unmapped conv.
// Same additive, denormalised shape as v77AgentColumns: the conv column STAYS
// (it names the exact generation — a forensic snapshot / focus target), and the
// agent column is the pruning-immune key attribution/routing reads at the
// boundary. Both keys coexist; this is not a cutover.
//
//   - human_messages.from_agent — the notify-human sender, for the Messages-tab
//     attribution (and a future focus-raise that survives a sender rotation).
//   - pending_spawns.reply_to_agent / spawned_by_agent — the startup-briefing
//     reply target and the spawn provenance, reconstructed minutes later by the
//     pending-spawn sweeper when the spawner may have rotated.
var v81CompanionAgentColumns = []struct {
	table    string
	agentCol string
	convCols []string // backfill source(s), COALESCE'd in order
}{
	{"human_messages", "from_agent", []string{"from_conv"}},
	{"pending_spawns", "reply_to_agent", []string{"reply_to_conv"}},
	{"pending_spawns", "spawned_by_agent", []string{"spawned_by_conv"}},
}

// migrateV80toV81 adds the v81CompanionAgentColumns and backfills them from
// agent_conversations. See that var's doc for the design; the mechanics mirror
// migrateV76toV77 exactly (it reuses backfillAgentColSQL).
//
// Additive + idempotent (the v76–v80 convention): each table is probed so a
// partial-schema heal DB missing one is a clean skip; each ADD COLUMN is guarded
// by a pragma_table_info probe so a half-applied run converges on re-run instead
// of wedging on "duplicate column"; the backfill UPDATE recomputes the same join
// (naturally idempotent) and is skipped when the agent_conversations resolution
// spine is absent. The whole pass runs in one transaction, so a failure rolls
// back and the version stays at 80. No VACUUM snapshot: nothing is dropped or
// rewritten.
func migrateV80toV81(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v80→v81: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveConvs, err := txTableExists(tx, "agent_conversations")
	if err != nil {
		return fmt.Errorf("migrate v80→v81 (probe agent_conversations): %w", err)
	}

	for _, spec := range v81CompanionAgentColumns {
		haveTable, err := txTableExists(tx, spec.table)
		if err != nil {
			return fmt.Errorf("migrate v80→v81 (probe %s): %w", spec.table, err)
		}
		if !haveTable {
			continue // partial-schema heal DB without this table — skip
		}

		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, spec.table, spec.agentCol,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v80→v81 (probe %s.%s): %w", spec.table, spec.agentCol, err)
		}
		if have == 0 {
			// Table + column names come from the hardcoded spec list above,
			// never user input.
			if _, err := tx.Exec(
				`ALTER TABLE ` + spec.table + ` ADD COLUMN ` + spec.agentCol + ` TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v80→v81 (add %s.%s): %w", spec.table, spec.agentCol, err)
			}
		}

		if haveConvs {
			// On a partial-schema heal DB the conv source column(s) may be
			// absent (a minimal seeded table). Backfill only over the ones that
			// exist, and skip entirely when none do — the ADD COLUMN above still
			// stood the agent column up.
			present := make([]string, 0, len(spec.convCols))
			for _, cc := range spec.convCols {
				var n int
				if err := tx.QueryRow(
					`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, spec.table, cc).Scan(&n); err != nil {
					return fmt.Errorf("migrate v80→v81 (probe %s.%s): %w", spec.table, cc, err)
				}
				if n > 0 {
					present = append(present, cc)
				}
			}
			if len(present) > 0 {
				if _, err := tx.Exec(backfillAgentColSQL(spec.table, spec.agentCol, present, false)); err != nil {
					return fmt.Errorf("migrate v80→v81 (backfill %s.%s): %w", spec.table, spec.agentCol, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 81`); err != nil {
		return fmt.Errorf("migrate v80→v81 (version): %w", err)
	}
	return tx.Commit()
}
