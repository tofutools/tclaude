package db

import (
	"fmt"
	"log/slog"
	"time"
)

// resolveCarriedName returns the agent's display name to carry across a
// conv-id rotation: its custom title, else its actor's spawn-time pending_name.
// "" when the agent has no name. Read before the rotation transaction — these
// are the source rows that are about to be advanced.
func resolveCarriedName(oldConv string) string {
	if row, err := GetConvIndex(oldConv); err == nil && row != nil && row.CustomTitle != "" {
		return row.CustomTitle
	}
	if a, err := GetAgentByConv(oldConv); err == nil && a != nil {
		return a.PendingName
	}
	return ""
}

// RotateAgentConv handles a conv-id rotation (reincarnate / Claude Code /clear).
//
// As of the JOH-26 agent-id cutover, EVERY identity-bearing table — group
// memberships, ownerships, permission overrides, sudo grants, notify prefs,
// cron jobs, and the spawn/clone rate-limit history — is keyed on the stable
// agent_id, so a rotation rekeys NOTHING: the actor keeps its agent_id and only
// its live conv pointer advances. This is the agents-table-only successor to the
// removed MigrateAgentIdentity, which also dual-wrote the now-deleted
// agent_enrollment table; a single source means no enrollment retire and no
// dual-write divergence.
//
// In one transaction it:
//
//   - records the succession edge (agent_conv_succession: old → new, so stale
//     references resolve forward via db.ResolveLatestConv — forensic / redirect
//     only, no longer an identity key); and
//   - advances the actor: links newConv as the fresh head generation of
//     oldConv's actor, moves the live pointer old → new, and carries the display
//     name onto the actor row (agents.pending_name) so the dashboard shows it
//     before the agent's own /rename lands.
//
// This is the shared core of the two conv-id rotations tclaude knows:
//
//   - `tclaude agent reincarnate` — a fresh CC process replaces the old.
//   - Claude Code's `/clear` — the SAME process rotates its conv-id, orphaning
//     every identity row that used to be keyed on the old one (issue #192).
//
// Atomic: the whole rotation commits or rolls back as a unit, so a failure can
// never leave an actor half-advanced. Idempotent: the succession upsert and the
// CAS-guarded pointer advance both converge on re-run, so a rotation that fails
// (a transient SQLite error) leaves oldConv wholly intact and can simply be
// retried — the retry predicate (needsIdentityMigration) keys on the succession
// edge, which a failed-and-rolled-back attempt never records.
//
// reason is the short succession tag ("reincarnate", "clear") — it is stored on
// the succession row and used as newConv's generation-link reason.
//
// Returns:
//
//   - carriedName — the agent's display name (also carried onto the actor), so
//     the caller can restore it as a real conversation title; for /clear, by
//     injecting /rename, since /clear wipes CC's own title.
//   - moved — whether the actor's live pointer actually advanced. false is a
//     clean skip (the successor is already owned by another actor, or oldConv is
//     no longer the live head): the succession edge still commits so stale refs
//     redirect, but the pointer/role advance is held back. Not an error.
//
// Callers must ensure oldConv is genuinely an agent — RotateAgentConv links
// newConv onto oldConv's actor unconditionally, so calling it for a plain
// conversation would wrongly fork the successor onto a freshly-minted actor.
func RotateAgentConv(oldConv, newConv, reason string) (carriedName string, moved bool, err error) {
	if oldConv == "" || newConv == "" {
		return "", false, fmt.Errorf("RotateAgentConv: oldConv and newConv must be non-empty")
	}
	if oldConv == newConv {
		return "", false, fmt.Errorf("RotateAgentConv: oldConv and newConv must differ")
	}

	// Resolve the display name before the transaction — it reads the old conv's
	// records, which the transaction below advances.
	carriedName = resolveCarriedName(oldConv)

	d, err := Open()
	if err != nil {
		return "", false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	nowSec := now.UTC().Format(time.RFC3339)

	// --- succession edge old → new ---
	// Mirrors db.RecordConvSuccession. Powers db.ResolveLatestConv, so a stale
	// reference to oldConv (a queued message, a CLI selector) resolves forward to
	// the live agent.
	if _, err := tx.Exec(`INSERT INTO agent_conv_succession
		(old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(old_conv_id) DO UPDATE SET
			new_conv_id  = excluded.new_conv_id,
			reason       = excluded.reason,
			succeeded_at = excluded.succeeded_at`,
		oldConv, newConv, reason, nowSec); err != nil {
		return "", false, fmt.Errorf("RotateAgentConv: record succession: %w", err)
	}

	// --- advance the actor ---
	// A rotation preserves the actor: link newConv to oldConv's agent_id as a
	// fresh generation and advance the live pointer, carrying the display name
	// onto the actor row. ensureAgentForConvTx is the defensive allocate for the
	// (unexpected) case where oldConv has no actor yet; in practice the caller
	// has confirmed oldConv is a live agent.
	agentID, err := ensureAgentForConvTx(tx, oldConv, reason)
	if err != nil {
		return "", false, fmt.Errorf("RotateAgentConv: resolve actor: %w", err)
	}
	moved, err = advanceAgentToNewConv(tx, agentID, oldConv, newConv, reason, carriedName, now)
	if err != nil {
		return "", false, fmt.Errorf("RotateAgentConv: advance actor: %w", err)
	}
	if !moved {
		slog.Warn("RotateAgentConv: actor pointer not advanced (successor owned elsewhere or oldConv not the live head)",
			"old", oldConv, "new", newConv, "agent", agentID)
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("RotateAgentConv: commit: %w", err)
	}
	return carriedName, moved, nil
}
