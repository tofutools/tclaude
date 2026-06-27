package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// v77AgentColumns lists the durable agent_id companion columns added in
// migrate v77 (JOH-27 PR4). Each entry adds one TEXT column to `table`,
// backfilled to the owning actor of the row's conversation reference(s):
// COALESCE over a SELECT against agent_conversations for each convCol in order
// (first non-empty wins), falling back to '' for a non-actor / unmapped conv.
//
// The conv columns STAY — they name a conversation generation (a delivery
// target, a content row, a cost period), which is correct. The agent column is
// an ADDITIVE, denormalised "which agent was involved" reference: durable
// across conv pruning and directly queryable without joining
// agent_conversations at read time. Both keys coexist; this is not a cutover
// (contrast v73/v74, which renamed conv refs to agent refs).
//
// Two flavours:
//   - Authors ("who did it"): actor / by columns — the actor's conv is enrolled
//     by the time the action is recorded, so derive-at-write covers new rows.
//   - Owners ("whose generation"): a row can be written before its conv enrolls
//     (a `sessions` row predates the hook that enrolls the agent), so enrollment
//     also propagates into these (linkConvTx / advanceAgentToNewConv) to fill
//     rows the insert-time derivation left as ''.
var v77AgentColumns = []struct {
	table    string
	agentCol string
	convCols []string // backfill source(s), COALESCE'd in order
}{
	// Authors — "who did it".
	{"audit_log", "actor_agent", []string{"actor_conv"}},
	{"audit_log", "target_agent", []string{"target_conv"}},
	{"agent_group_audit", "by_agent", []string{"by_conv"}},
	{"agent_group_links", "by_agent", []string{"by_conv"}},
	{"agent_transfer_log", "by_agent", []string{"by_conv"}},
	{"agent_head_aliases", "by_agent", []string{"by_conv"}},
	// Owners — "whose generation".
	{"agent_head_aliases", "anchor_agent_id", []string{"anchor_conv_id"}},
	{"session_cost_daily", "agent_id", []string{"conv_id"}},
	// old/new are two generations of the SAME actor; either resolves to it.
	// Prefer the live successor (new) and fall back to the predecessor (old).
	{"agent_conv_succession", "agent_id", []string{"new_conv_id", "old_conv_id"}},
	{"sessions", "agent_id", []string{"conv_id"}},
	{"agent_workdir", "agent_id", []string{"conv_id"}},
	{"agent_workspace", "agent_id", []string{"conv_id"}},
	{"export_jobs", "agent_id", []string{"conv_id"}},
	{"export_jobs", "worker_agent_id", []string{"worker_conv_id"}},
	{"ask_threads", "agent_id", []string{"conv_id"}},
}

// migrateV76toV77 adds the v77AgentColumns and backfills them from
// agent_conversations. See that var's doc for the design.
//
// Additive + idempotent (the v68→v69 / v76 convention): each table is probed so
// a partial-schema heal DB missing one is a clean skip; each ADD COLUMN is
// guarded by a pragma_table_info probe so a half-applied run converges on re-run
// instead of wedging on "duplicate column"; the backfill UPDATE recomputes the
// same join (naturally idempotent) and is skipped when the agent_conversations
// resolution spine is absent. The whole pass runs in one transaction, so a
// failure rolls back and the version stays at 76. No VACUUM snapshot: nothing is
// dropped or rewritten.
func migrateV76toV77(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v76→v77: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveConvs, err := txTableExists(tx, "agent_conversations")
	if err != nil {
		return fmt.Errorf("migrate v76→v77 (probe agent_conversations): %w", err)
	}

	for _, spec := range v77AgentColumns {
		haveTable, err := txTableExists(tx, spec.table)
		if err != nil {
			return fmt.Errorf("migrate v76→v77 (probe %s): %w", spec.table, err)
		}
		if !haveTable {
			continue // partial-schema heal DB without this table — skip
		}

		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, spec.table, spec.agentCol,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v76→v77 (probe %s.%s): %w", spec.table, spec.agentCol, err)
		}
		if have == 0 {
			// Table + column names come from the hardcoded spec list above,
			// never user input.
			if _, err := tx.Exec(
				`ALTER TABLE ` + spec.table + ` ADD COLUMN ` + spec.agentCol + ` TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v76→v77 (add %s.%s): %w", spec.table, spec.agentCol, err)
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
					return fmt.Errorf("migrate v76→v77 (probe %s.%s): %w", spec.table, cc, err)
				}
				if n > 0 {
					present = append(present, cc)
				}
			}
			if len(present) > 0 {
				if _, err := tx.Exec(backfillAgentColSQL(spec.table, spec.agentCol, present, false)); err != nil {
					return fmt.Errorf("migrate v76→v77 (backfill %s.%s): %w", spec.table, spec.agentCol, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 77`); err != nil {
		return fmt.Errorf("migrate v76→v77 (version): %w", err)
	}
	return tx.Commit()
}

// backfillAgentColSQL builds the UPDATE that resolves each conv reference to its
// owning agent_id via agent_conversations (conv_id is its PK, so each correlated
// subquery returns at most one row). COALESCE(..., '') keeps a non-actor /
// unmapped conv as '' rather than NULL, which the NOT NULL column would reject;
// the WHERE limits work to rows that actually carry a conv reference. This is
// the same derivation the insert-time dual-write uses, so backfilled and freshly
// written rows agree. Table/column names are caller-hardcoded, never user input.
//
// onlyEmpty restricts the update to rows whose agent column is still '' — used
// by the group-import path so re-deriving never blanks a pre-existing local
// row whose actor has since been unmapped. The v77 migration passes false (the
// columns are freshly added and uniformly '', so the guard would be a no-op).
func backfillAgentColSQL(table, agentCol string, convCols []string, onlyEmpty bool) string {
	subs := make([]string, len(convCols))
	conds := make([]string, len(convCols))
	for i, cc := range convCols {
		subs[i] = `(SELECT agent_id FROM agent_conversations WHERE conv_id = ` + table + `.` + cc + `)`
		conds[i] = table + `.` + cc + ` != ''`
	}
	where := strings.Join(conds, " OR ")
	if onlyEmpty {
		where = `(` + where + `) AND ` + table + `.` + agentCol + ` = ''`
	}
	return `UPDATE ` + table + ` SET ` + agentCol +
		` = COALESCE(` + strings.Join(subs, ", ") + `, '')` +
		` WHERE ` + where
}
