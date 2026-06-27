package db

import (
	"database/sql"
	"errors"
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
// Returns carriedName — the agent's display name (also carried onto the actor)
// — so the caller can restore it as a real conversation title; for /clear, by
// injecting /rename, since /clear wipes CC's own title.
//
// Failure contract: an error means the rotation did NOT take effect and nothing
// was committed (the whole transaction rolls back). In particular, if the
// actor's live pointer cannot be advanced onto newConv — the successor already
// owns an actor that is NOT a bare self-registration (so it cannot be safely
// absorbed), or oldConv is no longer the actor's head and newConv is not already
// the head — that is a FAILED rotation, not a silent partial one: it returns an
// error rather than committing a succession edge whose actor head is elsewhere.
// This makes the two lifecycle callers correct: reincarnate aborts (leaving the
// old pane intact) and /clear retries on the next hook.
//
// Callers must ensure oldConv is genuinely an agent — RotateAgentConv links
// newConv onto oldConv's actor unconditionally, so calling it for a plain
// conversation would wrongly fork the successor onto a freshly-minted actor.
func RotateAgentConv(oldConv, newConv, reason string) (carriedName string, err error) {
	if oldConv == "" || newConv == "" {
		return "", fmt.Errorf("RotateAgentConv: oldConv and newConv must be non-empty")
	}
	if oldConv == newConv {
		return "", fmt.Errorf("RotateAgentConv: oldConv and newConv must differ")
	}

	// Resolve the display name before the transaction — it reads the old conv's
	// records, which the transaction below advances.
	carriedName = resolveCarriedName(oldConv)

	d, err := Open()
	if err != nil {
		return "", err
	}
	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	nowSec := now.UTC().Format(time.RFC3339)

	// --- succession edge old → new ---
	// Mirrors db.RecordConvSuccession. Powers db.ResolveLatestConv, so a stale
	// reference to oldConv (a queued message, a CLI selector) resolves forward to
	// the live agent. Committed only when the actor advance below also succeeds.
	if _, err := tx.Exec(`INSERT INTO agent_conv_succession
		(old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(old_conv_id) DO UPDATE SET
			new_conv_id  = excluded.new_conv_id,
			reason       = excluded.reason,
			succeeded_at = excluded.succeeded_at`,
		oldConv, newConv, reason, nowSec); err != nil {
		return "", fmt.Errorf("RotateAgentConv: record succession: %w", err)
	}

	// --- advance the actor ---
	// A rotation preserves the actor: link newConv to oldConv's agent_id as a
	// fresh generation and advance the live pointer, carrying the display name
	// onto the actor row. ensureAgentForConvTx is the defensive allocate for the
	// (unexpected) case where oldConv has no actor yet; in practice the caller
	// has confirmed oldConv is a live agent.
	agentID, err := ensureAgentForConvTx(tx, oldConv, reason)
	if err != nil {
		return "", fmt.Errorf("RotateAgentConv: resolve actor: %w", err)
	}
	// Absorb a reincarnate successor's bare self-registered actor so newConv is
	// free to link onto the predecessor's actor (see absorbBareSuccessorActorTx).
	if absorbed, err := absorbBareSuccessorActorTx(tx, agentID, newConv); err != nil {
		return "", fmt.Errorf("RotateAgentConv: absorb successor: %w", err)
	} else if absorbed {
		slog.Info("RotateAgentConv: absorbed the successor's bare self-registered actor",
			"old", oldConv, "new", newConv, "agent", agentID)
	}
	moved, err := advanceAgentToNewConv(tx, agentID, oldConv, newConv, reason, carriedName, now)
	if err != nil {
		return "", fmt.Errorf("RotateAgentConv: advance actor: %w", err)
	}
	if !moved {
		// The pointer did not advance. Tolerate the idempotent already-done case
		// (newConv is ALREADY this actor's live head — a re-run), but treat a
		// genuine failure-to-advance as an error so the whole tx rolls back
		// (succession edge included) rather than leaving the actor head elsewhere.
		cur, err := currentConvForAgentTx(tx, agentID)
		if err != nil {
			return "", fmt.Errorf("RotateAgentConv: read actor head: %w", err)
		}
		if cur != newConv {
			return "", fmt.Errorf("RotateAgentConv: actor %s did not advance to %s (head=%s) — successor owned elsewhere or oldConv not the live head",
				agentID, newConv, cur)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("RotateAgentConv: commit: %w", err)
	}
	return carriedName, nil
}

// currentConvForAgentTx reads an actor's live conv pointer inside a transaction.
func currentConvForAgentTx(tx dbExecQuerier, agentID string) (string, error) {
	var cur string
	err := tx.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id = ?`, agentID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cur, err
}
