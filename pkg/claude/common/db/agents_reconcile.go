package db

import "fmt"

// ReconcileAgentRoster heals drift between the legacy conv-keyed
// agent_enrollment roster and the stable actor-level agents roster on a DB
// already at head. Since JOH-26 PR3b the roster surfaces read ONLY the agents
// table, but the enrollment→actor dual-write (EnrollAgent / RetireAgent /
// ReinstateAgent / PromoteAgent) is best-effort — it logs and proceeds when the
// actor write fails — and backfillAgents otherwise only runs at a migration
// threshold. So a transient failure on a head DB could leave persistent drift:
//
//   - an active enrollment with no agents row → the agent drops off the active
//     roster (and could leak into the plain-conversations list); and
//   - a half-applied retire/reinstate (enrollment flipped, actor not) → the
//     agent shows in the wrong roster bucket, stuck (a re-retire is a no-op).
//
// This runs the repair idempotently, once, at daemon startup — self-healing in
// the load path rather than a one-shot migration, so it converges on every run:
//
//  1. backfillAgents — every conv referenced by an identity table (incl. each
//     active enrollment) gets an actor row, healing a missing-actor drop-off.
//  2. retired-flag sync — each actor's retire state is mirrored from its CURRENT
//     generation's enrollment, the human-intent source the retire/promote
//     handlers still gate on. A predecessor generation's enrollment is retired
//     by a rotation but its actor stays active, so ONLY the current conv decides
//     — the sync keys strictly on agents.current_conv_id.
//
// The whole reconcile is conv-keyed-enrollment-aware on purpose; it retires
// together with agent_enrollment itself in the follow-up cleanup (PR3c), after
// which the agents table is the sole source and no reconcile is needed.
//
// Best-effort by contract: a failure is returned for the caller to log, never
// fatal — a daemon must still start if the heal hiccups.
func ReconcileAgentRoster() error {
	d, err := Open()
	if err != nil {
		return err
	}
	// 1. Heal missing actors (idempotent — skips already-mapped convs).
	if err := backfillAgents(d); err != nil {
		return fmt.Errorf("reconcile agent roster (backfill): %w", err)
	}
	// 2. Sync each actor's retire state from its current generation's
	// enrollment, but only where the two diverge and an enrollment row exists
	// for the current conv. agent_enrollment.retired_at is "" for active, an
	// RFC3339Nano stamp for retired — copying all three retire columns keeps the
	// audit fields (who / why) consistent too.
	if ok, err := tableExists(d, "agent_enrollment"); err != nil {
		return fmt.Errorf("reconcile agent roster (probe enrollment): %w", err)
	} else if !ok {
		return nil // enrollment already gone (post-PR3c) — agents is sole source
	}
	if _, err := d.Exec(`
		UPDATE agents
		SET retired_at    = (SELECT e.retired_at    FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id),
		    retired_by    = (SELECT e.retired_by    FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id),
		    retire_reason = (SELECT e.retire_reason FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id)
		WHERE current_conv_id IN (SELECT conv_id FROM agent_enrollment)
		  AND retired_at != (SELECT e.retired_at FROM agent_enrollment e WHERE e.conv_id = agents.current_conv_id)`,
	); err != nil {
		return fmt.Errorf("reconcile agent roster (retired-flag sync): %w", err)
	}
	return nil
}
