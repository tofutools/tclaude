package db

import (
	"database/sql"
	"fmt"
)

// migrateV75toV76 adds the durable actor columns agent_messages.from_agent /
// to_agent (JOH-27 PR3a) — the stable agent_id of each message's sender and
// recipient, denormalised alongside the existing from_conv / to_conv.
//
// from_conv / to_conv STAY: delivery still targets a specific conversation
// generation (a nudge lands on the live tmux pane, which is a conv, not an
// actor). The new columns are an ADDITIVE actor reference so message display
// (PR3b) can render `name (agent_id)` straight from the row — survives conv
// pruning and never has to resolve conv→agent at render time. This is NOT a
// cutover (contrast v73/v74, which renamed conv refs to agent refs): a message
// is forever tied to the generation it was delivered to, so both keys coexist.
//
// A message can legitimately reference a non-actor conv — a plain conversation,
// a since-deleted agent, or a conv that was never enrolled — so the backfill is
// best-effort: an unmapped conv leaves the agent column '' (NOT NULL DEFAULT '').
// There is no strict coverage gate here (contrast v74's unmappedV74Rows abort):
// blanking is the correct outcome for a non-actor ref, not data loss. The send
// path (db.InsertAgentMessage) dual-writes the same derivation going forward.
//
// Additive + idempotent (the v68→v69 convention): the table is probed first so a
// partial-schema heal DB without agent_messages is a clean no-op; each ADD COLUMN
// is guarded by a pragma_table_info probe so a half-applied run converges on
// re-run instead of wedging on "duplicate column"; the backfill UPDATE is
// naturally idempotent (it recomputes the same join) and is skipped if the
// agent_conversations resolution spine is absent. The whole pass runs in one
// transaction, so a failure rolls back and the version stays at 75. No VACUUM
// snapshot: nothing is dropped or rewritten, so there is nothing to restore.
func migrateV75toV76(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v75→v76: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveMessages, err := txTableExists(tx, "agent_messages")
	if err != nil {
		return fmt.Errorf("migrate v75→v76 (probe agent_messages): %w", err)
	}
	if !haveMessages {
		// Partial-schema heal DB with no agent_messages — nothing to add.
		if _, err := tx.Exec(`UPDATE schema_version SET version = 76`); err != nil {
			return fmt.Errorf("migrate v75→v76 (version, no-op): %w", err)
		}
		return tx.Commit()
	}

	for _, col := range []string{"from_agent", "to_agent"} {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = ?`, col,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v75→v76 (probe agent_messages.%s): %w", col, err)
		}
		if have > 0 {
			continue // already added by a prior partial run
		}
		// Column / table names come from this hardcoded list, never user input.
		if _, err := tx.Exec(
			`ALTER TABLE agent_messages ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migrate v75→v76 (add agent_messages.%s): %w", col, err)
		}
	}

	// Backfill each non-empty conv ref to its owning actor. agent_conversations
	// is the resolution spine v72 created; a partial-schema heal DB could lack
	// it, in which case the columns just stay '' (the send path repopulates as
	// messages flow). agent_conversations has conv_id as its PK, so the
	// correlated subquery returns at most one row; COALESCE(...,'') keeps a
	// non-actor / unmapped conv as '' rather than NULL (which the NOT NULL column
	// would reject). This is the same join the send path dual-writes, so
	// backfilled and freshly-inserted rows agree.
	haveConvs, err := txTableExists(tx, "agent_conversations")
	if err != nil {
		return fmt.Errorf("migrate v75→v76 (probe agent_conversations): %w", err)
	}
	if haveConvs {
		for _, c := range []struct{ agentCol, convCol string }{
			{"from_agent", "from_conv"},
			{"to_agent", "to_conv"},
		} {
			if _, err := tx.Exec(
				`UPDATE agent_messages SET ` + c.agentCol +
					` = COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = agent_messages.` + c.convCol + `), '')` +
					` WHERE ` + c.convCol + ` != ''`,
			); err != nil {
				return fmt.Errorf("migrate v75→v76 (backfill %s): %w", c.agentCol, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 76`); err != nil {
		return fmt.Errorf("migrate v75→v76 (version): %w", err)
	}
	return tx.Commit()
}

// txTableExists reports whether a table exists, using the migration's open
// transaction (the package's tableExists helper takes a *sql.DB and cannot see
// uncommitted DDL on tx). Name comes from a hardcoded caller, never user input.
func txTableExists(tx *sql.Tx, name string) (bool, error) {
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
