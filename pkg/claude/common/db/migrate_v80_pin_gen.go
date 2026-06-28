package db

import (
	"database/sql"
	"fmt"
)

// migrateV79toV80 adds agent_messages.pin_gen (JOH-310): a 0/1 flag marking a
// message deliberately addressed to a SPECIFIC previous generation of the
// recipient agent (the sender passed an explicit `gen` conv-id), as opposed to
// the normal case where a message follows the agent to its current head
// generation at delivery time.
//
// Motivation: the async per-agent delivery queue keys delivery on the stable
// agent_id and resolves the agent's CURRENT head conv at drain time, so a
// message queued before a reincarnate/`/clear` still reaches the live
// generation. pin_gen is the opt-out: a pinned message sticks to its recorded
// to_conv instead of following the head, so the agent-keyed drain must exclude
// it (and the exact-conv drain must own it). DEFAULT 0 makes every existing row
// head-following — the historical behaviour — so no backfill is needed.
//
// Additive + idempotent (the v76–v79 convention): the agent_messages probe
// makes a partial-schema heal DB a clean skip; the pragma_table_info guard lets
// a half-applied run converge instead of wedging on "duplicate column". The
// whole pass runs in one transaction, so a failure rolls back and the version
// stays at 79. No VACUUM/snapshot: nothing is dropped or rewritten.
func migrateV79toV80(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v79→v80: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveMessages, err := txTableExists(tx, "agent_messages")
	if err != nil {
		return fmt.Errorf("migrate v79→v80 (probe agent_messages): %w", err)
	}
	if !haveMessages {
		// Partial-schema heal DB with no agent_messages — nothing to add.
		if _, err := tx.Exec(`UPDATE schema_version SET version = 80`); err != nil {
			return fmt.Errorf("migrate v79→v80 (version, no-op): %w", err)
		}
		return tx.Commit()
	}

	var have int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = 'pin_gen'`,
	).Scan(&have); err != nil {
		return fmt.Errorf("migrate v79→v80 (probe agent_messages.pin_gen): %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE agent_messages ADD COLUMN pin_gen INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migrate v79→v80 (add agent_messages.pin_gen): %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 80`); err != nil {
		return fmt.Errorf("migrate v79→v80 (version): %w", err)
	}
	return tx.Commit()
}
