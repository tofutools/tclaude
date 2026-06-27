package db

import (
	"database/sql"
	"fmt"
)

// migrateV74toV75 performs the FINAL enrollment→actor heal, then drops
// agent_enrollment — the conv-keyed "this conv is an agent" table that the
// stable actor layer (agents + agent_conversations, JOH-26) fully superseded.
// By v75 every production reader has moved to the agents table (PR3c): the
// identity/roster role is the agents table's, the tri-state probe is
// db.AgentState, retire state is agents.retired_at, the spawn-time display
// fallback is agents.pending_name, and the succession-derived generations are
// agent_conversations rows.
//
// HEAL BEFORE DROP (the load-bearing part). On a head DB upgraded from PR3b the
// enrollment→actor dual-write was best-effort, so agent_enrollment and the
// agents table can have drifted: an active enrollment with no agents row, or a
// retired/active enrollment whose actor never got the matching flip. PR3b healed
// this at every daemon startup via ReconcileAgentRoster; PR3c removes that
// startup reconcile (single source ⇒ nothing to reconcile going forward), so the
// authoritative enrollment facts must be folded into the agents table ONE LAST
// TIME here — before the table is dropped and the drift becomes unrecoverable.
// This runs the same two-step repair ReconcileAgentRoster did:
//
//  1. backfillAgents — every conv referenced by an identity table (incl. each
//     active enrollment) gets an actor row, healing a missing-actor drop-off.
//  2. retired-flag sync — each actor's retire state is mirrored from its CURRENT
//     generation's enrollment (keyed strictly on agents.current_conv_id, the
//     human-intent source the retire/promote handlers gated on). A predecessor
//     generation's enrollment was retired by a rotation, but its actor stays
//     active — so ONLY the current conv decides, and a predecessor's retired
//     enrollment never drags the live actor onto the retired roster.
//
// Ordering: this is the LAST link in the chain, so every earlier migration that
// references agent_enrollment (it is created at v30, read by the v30/v72/this
// backfill) still sees the table when IT runs. A fresh DB walks the whole chain
// — create at v30, heal+drop here — which is correct (the heal is a no-op on a
// fresh DB with no drift).
//
// Guarded + idempotent: backfillAgents skips already-mapped convs; the sync only
// touches actors whose state diverges; DROP … IF EXISTS converges whether or not
// the table is present, so a re-run (or a partial-schema heal) is a clean no-op.
// Nothing FK-references agent_enrollment, so the drop cascades nothing. A VACUUM
// INTO snapshot is written first — the same convenience insurance the v73/v74
// destructive migrations write (the operator's own backups remain the ultimate
// safety net).
func migrateV74toV75(db *sql.DB) error {
	vacuumBackup(db, ".pre-v75.bak")

	// --- final heal: fold the authoritative enrollment facts into the actor
	// layer before the table is gone (mirrors PR3b's ReconcileAgentRoster). ---
	if ok, err := tableExists(db, "agent_enrollment"); err != nil {
		return fmt.Errorf("migrate v74→v75 (probe enrollment): %w", err)
	} else if ok {
		// 1. Heal missing actors (idempotent — skips already-mapped convs).
		if err := backfillAgents(db); err != nil {
			return fmt.Errorf("migrate v74→v75 (backfill): %w", err)
		}
		// 2. Sync each actor's retire state from its CURRENT generation's
		// enrollment, only where the two diverge and an enrollment row exists
		// for the current conv. Copying all three retire columns keeps the audit
		// fields (who / why) consistent too.
		if _, err := db.Exec(`
			UPDATE agents
			SET retired_at    = (SELECT e.retired_at    FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id),
			    retired_by    = (SELECT e.retired_by    FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id),
			    retire_reason = (SELECT e.retire_reason FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id)
			WHERE current_conv_id IN (SELECT conv_id FROM agent_enrollment)
			  AND retired_at != (SELECT e.retired_at FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id)`,
		); err != nil {
			return fmt.Errorf("migrate v74→v75 (retire-flag sync): %w", err)
		}
	}

	// --- drop, now that the agents table carries every fact ---
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_agent_enrollment_active`); err != nil {
		return fmt.Errorf("migrate v74→v75 (drop index): %w", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_enrollment`); err != nil {
		return fmt.Errorf("migrate v74→v75 (drop table): %w", err)
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = 75`); err != nil {
		return fmt.Errorf("migrate v74→v75 (version): %w", err)
	}
	return nil
}
