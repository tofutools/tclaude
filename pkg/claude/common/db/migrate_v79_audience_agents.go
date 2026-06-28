package db

import (
	"database/sql"
	"fmt"
)

// migrateV78toV79 adds the audience-agent companion columns
// agent_messages.to_recipient_agents / cc_recipient_agents (JOH-284) — the
// stable agent_id arrays denormalised alongside the existing to_recipients /
// cc_recipients conv-id arrays, indexed 1:1 with them.
//
// Motivation: the email-style To:/CC: audience was stored as conv-ids only and
// re-resolved conv→agent at READ time. When a recipient's conversation
// generation is pruned (DeleteAgentByConvID), that conv→agent link is gone, so
// the old read-time resolution silently dropped the stable id from a historical
// message's audience even though the actor still exists. Persisting the agent
// ids at insert (and snapshotting the existing ones here) makes the audience
// survive pruning, the same way from_agent / to_agent (v76) made the single
// sender/recipient survive it. This mirrors v76's companion-column shape exactly
// — additive, the conv arrays STAY (a multi-recipient send is forever the
// audience it was delivered to), both coexist.
//
// Backfill is per-element over a JSON array, which a scalar SQL subquery can't
// express, so it runs in Go: each row's conv array is resolved element-by-element
// through agent_conversations (the same join v76 / the send path use) into the
// parallel agent array. Best-effort: a non-actor / already-pruned conv resolves
// to "" and the reader falls back to a live lookup on that slot; a row whose
// audience resolves to no actors at all keeps the empty-string default and the
// reader falls back wholesale (unchanged behaviour for legacy rows).
//
// Additive + idempotent (the v76 / v77 / v78 convention): the agent_messages
// probe makes a partial-schema heal DB a clean skip; the pragma_table_info guard
// lets a half-applied run converge on re-run instead of wedging on "duplicate
// column"; the backfill recomputes the same resolution (idempotent) and is
// skipped when the agent_conversations spine is absent. The whole pass runs in
// one transaction, so a failure rolls back and the version stays at 78. No VACUUM
// snapshot: nothing is dropped or rewritten.
func migrateV78toV79(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v78→v79: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveMessages, err := txTableExists(tx, "agent_messages")
	if err != nil {
		return fmt.Errorf("migrate v78→v79 (probe agent_messages): %w", err)
	}
	if !haveMessages {
		// Partial-schema heal DB with no agent_messages — nothing to add.
		if _, err := tx.Exec(`UPDATE schema_version SET version = 79`); err != nil {
			return fmt.Errorf("migrate v78→v79 (version, no-op): %w", err)
		}
		return tx.Commit()
	}

	for _, col := range []string{"to_recipient_agents", "cc_recipient_agents"} {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = ?`, col,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v78→v79 (probe agent_messages.%s): %w", col, err)
		}
		if have > 0 {
			continue // already added by a prior partial run
		}
		// Column names come from this hardcoded list, never user input.
		if _, err := tx.Exec(
			`ALTER TABLE agent_messages ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migrate v78→v79 (add agent_messages.%s): %w", col, err)
		}
	}

	// Backfill only when the resolution spine (agent_conversations) is present —
	// a partial-schema heal DB may lack it, in which case the columns just stay
	// '' (the send path repopulates as messages flow).
	haveConvs, err := txTableExists(tx, "agent_conversations")
	if err != nil {
		return fmt.Errorf("migrate v78→v79 (probe agent_conversations): %w", err)
	}
	if haveConvs {
		if err := backfillAudienceAgents(tx); err != nil {
			return fmt.Errorf("migrate v78→v79 (backfill audience agents): %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 79`); err != nil {
		return fmt.Errorf("migrate v78→v79 (version): %w", err)
	}
	return tx.Commit()
}

// backfillAudienceAgents snapshots the stable agent_id companion arrays for every
// message that carries an audience, resolving each conv-id element to its owning
// actor via agent_conversations. It reads the whole working set into memory FIRST
// (closing the cursor) before issuing any UPDATE, so the read and the writes never
// share an open cursor on the same transaction. Rows whose audience resolves to no
// actor are left with the empty-string default (recipientAgentsJSON returns "").
func backfillAudienceAgents(tx *sql.Tx) error {
	type row struct {
		id      int64
		toConvs []string
		ccConvs []string
	}
	rows, err := tx.Query(`SELECT id, to_recipients, cc_recipients
		FROM agent_messages
		WHERE to_recipients != '' OR cc_recipients != ''`)
	if err != nil {
		return err
	}
	var work []row
	for rows.Next() {
		var id int64
		var toJSON, ccJSON string
		if err := rows.Scan(&id, &toJSON, &ccJSON); err != nil {
			_ = rows.Close()
			return err
		}
		work = append(work, row{
			id:      id,
			toConvs: recipientsFromJSON(toJSON),
			ccConvs: recipientsFromJSON(ccJSON),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, w := range work {
		toAgents := recipientAgentsJSON(tx, w.toConvs)
		ccAgents := recipientAgentsJSON(tx, w.ccConvs)
		if toAgents == "" && ccAgents == "" {
			continue // nothing resolved — leave the '' defaults
		}
		if _, err := tx.Exec(
			`UPDATE agent_messages SET to_recipient_agents = ?, cc_recipient_agents = ? WHERE id = ?`,
			toAgents, ccAgents, w.id); err != nil {
			return err
		}
	}
	return nil
}
