package db

import (
	"fmt"
	"log/slog"
	"time"
)

// AgentIdentityMigration is the outcome of MigrateAgentIdentity — the
// per-table count of rows rekeyed old → new, plus the agent's carried
// display name. Items is a compact human-readable summary (only the
// non-zero facets) for API responses and logs.
//
// As of the JOH-26 PR3a cutover (v74) NO count field is ever populated: every
// identity-bearing table — including cron jobs and the spawn/clone rate-limit
// history — is agent-keyed, so a rotation rekeys nothing. The count fields are
// retained (so summarize/Items and any API shape stay stable) but are always
// zero now; they go away with MigrateAgentIdentity itself in PR3b.
type AgentIdentityMigration struct {
	GroupMembers int64  // always 0 post-v73 (agent-keyed, no rekey)
	Ownerships   int64  // always 0 post-v73 (agent-keyed, no rekey)
	Permissions  int64  // always 0 post-v73 (agent-keyed, no rekey)
	CronJobs     int64  // always 0 post-v74 (agent-keyed, no rekey)
	NotifyPrefs  int64  // always 0 post-v73 (agent-keyed, no rekey)
	CarriedName  string // the agent's display name, carried onto newConv
	Items        []string
}

// summarize fills Items from the non-zero counts.
func (m *AgentIdentityMigration) summarize() {
	m.Items = []string{}
	add := func(kind string, n int64) {
		if n > 0 {
			m.Items = append(m.Items, fmt.Sprintf("%s:%d", kind, n))
		}
	}
	add("group_members", m.GroupMembers)
	add("permissions", m.Permissions)
	add("ownerships", m.Ownerships)
	add("cron_jobs", m.CronJobs)
	add("notify_prefs", m.NotifyPrefs)
}

// resolveCarriedName returns the agent's display name to carry across a
// conv-id rotation: its custom title, else its spawn-time pending_name.
// "" when the agent has no name. Read before the migration transaction
// — these are the source rows that are about to be rekeyed / dropped.
func resolveCarriedName(oldConv string) string {
	if row, err := GetConvIndex(oldConv); err == nil && row != nil && row.CustomTitle != "" {
		return row.CustomTitle
	}
	if e, err := GetEnrollment(oldConv); err == nil && e != nil {
		return e.PendingName
	}
	return ""
}

// MigrateAgentIdentity handles a conv-id rotation (reincarnate / /clear).
//
// As of the JOH-26 agent-id cutover (v73 + PR3a's v74), EVERY identity-bearing
// table — group memberships, ownerships, permission overrides, sudo grants,
// notify prefs, cron jobs, and the spawn/clone rate-limit history — is keyed on
// the stable agent_id, so a rotation rekeys NOTHING: the actor keeps its
// agent_id and only its live conv pointer advances (the agent dual-write below).
//
// Within the same transaction it records the succession edge
// (agent_conv_succession: old → new, so stale references resolve
// forward via db.ResolveLatestConv), enrolls newConv as an agent,
// carries the agent's display name onto newConv
// (agent_enrollment.pending_name), and RETIRES oldConv's enrollment
// (the old conv is superseded — its identity now lives on newConv;
// retiring instead of deleting lands it on the retired-agents roster
// so a human can reinstate it later for knowledge pings).
//
// This is the shared core of the two conv-id rotations tclaude knows:
//
//   - `tclaude agent reincarnate` — a fresh CC process replaces the old.
//   - Claude Code's `/clear` — the SAME process rotates its conv-id,
//     orphaning every identity row keyed on the old one (issue #192).
//
// Atomic: the whole migration commits or rolls back as a unit, so a
// failure can never leave an agent half-migrated (some memberships
// moved, others stranded) — the worst outcome of a partial migration.
// Idempotent: every statement is a rekey UPDATE / INSERT-OR-IGNORE /
// upsert keyed on conv-id, so a re-run converges on the same state. A
// migration that fails (a transient SQLite error) leaves oldConv wholly
// intact and can simply be retried.
//
// reason is the short succession tag ("reincarnate", "clear") — it is
// stored on the succession row and used as newConv's enrollment `via`.
// granter is the audit string (e.g. "system:reincarnate", "system:clear")
// — retained for the audit-trail convention clone / groups-create follow,
// though as of the cutover no per-row granted_by is rewritten here.
//
// The carried name is returned (AgentIdentityMigration.CarriedName) so
// the caller can also restore it as a real conversation title — for
// /clear, by injecting /rename, since /clear wipes CC's own title.
//
// Callers must ensure oldConv is genuinely an agent — MigrateAgentIdentity
// enrolls newConv unconditionally, so calling it for a plain conversation
// would wrongly promote the successor to an agent.
func MigrateAgentIdentity(oldConv, newConv, reason, granter string) (AgentIdentityMigration, error) {
	var out AgentIdentityMigration
	if oldConv == "" || newConv == "" {
		return out, fmt.Errorf("MigrateAgentIdentity: oldConv and newConv must be non-empty")
	}
	if oldConv == newConv {
		return out, fmt.Errorf("MigrateAgentIdentity: oldConv and newConv must differ")
	}

	// Resolve the display name before the transaction — it reads the
	// old conv's records, which the transaction below rekeys / drops.
	carriedName := resolveCarriedName(oldConv)

	d, err := Open()
	if err != nil {
		return out, err
	}
	tx, err := d.Begin()
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	nowNano := now.Format(time.RFC3339Nano)
	nowSec := now.UTC().Format(time.RFC3339)

	// --- no identity rows need rekeying (JOH-26 PR3a) ---
	//
	// Every identity-bearing table is now keyed on the stable agent_id: group
	// memberships / ownerships / permission overrides / notify pref (v73), and —
	// as of PR3a — cron jobs (owner_agent / target_agent) and the spawn/clone
	// rate-limit history. A rotation keeps the actor's agent_id and only advances
	// its live conv pointer (the dual-write below), so NOTHING here is physically
	// rekeyed. out.CronJobs stays 0; the field is retained for API/summary
	// stability and goes away with MigrateAgentIdentity itself in PR3b. The cron
	// fire path resolves owner/target agent → current_conv at fire time.

	// --- succession edge old → new ---
	// Mirrors db.RecordConvSuccession. Powers db.ResolveLatestConv, so a
	// stale reference to oldConv (a queued message, a CLI selector)
	// resolves forward to the live agent.
	if _, err := tx.Exec(`INSERT INTO agent_conv_succession
		(old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(old_conv_id) DO UPDATE SET
			new_conv_id  = excluded.new_conv_id,
			reason       = excluded.reason,
			succeeded_at = excluded.succeeded_at`,
		oldConv, newConv, reason, nowSec); err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: record succession: %w", err)
	}

	// --- enroll newConv as an agent ---
	// INSERT-OR-IGNORE: a no-op when an earlier run already enrolled it.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_enrollment
		(conv_id, enrolled_at, enrolled_via) VALUES (?, ?, ?)`,
		newConv, nowNano, reason); err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: enroll new conv: %w", err)
	}

	// --- stable agent-identity dual-write (JOH-26) ---
	// A rotation (reincarnate / /clear) preserves the actor: link newConv to
	// oldConv's agent_id as a fresh generation and advance the live pointer,
	// carrying the display name onto the actor row too. The whole dual-write
	// is BEST-EFFORT and non-aborting: it can skip (logged) but must never
	// roll back the legacy conv-keyed rekey above, which is the
	// behaviour-preserving contract of this additive release.
	//
	// It runs inside a SAVEPOINT so that a real DB error mid-dual-write rolls
	// back ONLY its own partial writes (e.g. a stray agents row whose link
	// never landed) — the legacy rekey stays committed-pending and the actor
	// layer is left clean, to re-sync on the next enroll. A clean "skip"
	// (successor owned elsewhere, or oldConv not the live head) is NOT an
	// error: it is released, keeping the generation link. Authorization still
	// reads the rekeyed rows; once authz cuts over to agent_id, this link is
	// what makes the rekey unnecessary.
	const dualWriteSP = "joh26_dualwrite"
	if _, err := tx.Exec("SAVEPOINT " + dualWriteSP); err != nil {
		slog.Warn("MigrateAgentIdentity: could not open dual-write savepoint; skipping actor dual-write",
			"old", oldConv, "new", newConv, "error", err)
	} else {
		dwErr := func() error {
			agentID, e := ensureAgentForConvTx(tx, oldConv, reason)
			if e != nil {
				return e
			}
			moved, e := advanceAgentToNewConv(tx, agentID, oldConv, newConv, reason, carriedName, now)
			if e != nil {
				return e
			}
			if !moved {
				slog.Warn("MigrateAgentIdentity: actor pointer not advanced (successor owned elsewhere or oldConv not the live head)",
					"old", oldConv, "new", newConv, "agent", agentID)
			}
			return nil
		}()
		if dwErr != nil {
			slog.Warn("MigrateAgentIdentity: actor dual-write failed; rolling back dual-write only (legacy rekey unaffected)",
				"old", oldConv, "new", newConv, "error", dwErr)
			if _, e := tx.Exec("ROLLBACK TO " + dualWriteSP); e != nil {
				return out, fmt.Errorf("MigrateAgentIdentity: rollback dual-write savepoint: %w", e)
			}
		}
		if _, e := tx.Exec("RELEASE " + dualWriteSP); e != nil {
			return out, fmt.Errorf("MigrateAgentIdentity: release dual-write savepoint: %w", e)
		}
	}

	// --- carry the display name onto newConv.pending_name ---
	// The post-rotation conv has no customTitle turn in its .jsonl, so
	// agent.FreshTitle would fall through to "(unknown)" without this.
	// pending_name is the rescan-immune carrier FreshTitle consults as
	// the pre-/rename fallback; conv_index.custom_title is NOT — it is
	// re-derived from the .jsonl on every scan, so a copy there would be
	// wiped on first rescan. For /clear the caller additionally injects
	// a /rename, which restores the title for good; pending_name covers
	// the dashboard until that lands. Skipped when the agent is unnamed.
	if carriedName != "" {
		if _, err := tx.Exec(
			`UPDATE agent_enrollment SET pending_name = ? WHERE conv_id = ?`,
			carriedName, newConv); err != nil {
			return out, fmt.Errorf("MigrateAgentIdentity: carry display name: %w", err)
		}
	}

	// --- retire the superseded old conv's enrollment ---
	// Its identity has moved to newConv. We RETIRE (not delete) so the
	// old conv lands on the retired-agents roster and stays
	// reinstatable — the human can revisit the pre-rotation
	// conversation later without resurrecting it as an active agent.
	// Active-state read paths use `retired_at = ''` as the filter, so
	// retiring is enough to drop the conv off live surfaces; the
	// `WHERE retired_at = ''` guard makes the UPDATE idempotent (a
	// re-run never overwrites a prior retire's audit fields). The
	// retire_reason names the successor's short8 + the path tag so
	// the chain is scannable at a glance in the Retired tray.
	short8 := newConv
	if len(short8) > 8 {
		short8 = short8[:8]
	}
	retireReason := fmt.Sprintf("superseded by %s (%s)", short8, reason)
	if _, err := tx.Exec(`UPDATE agent_enrollment
		SET retired_at = ?, retired_by = ?, retire_reason = ?
		WHERE conv_id = ? AND retired_at = ''`,
		nowNano, granter, retireReason, oldConv); err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: retire old enrollment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AgentIdentityMigration{}, fmt.Errorf("MigrateAgentIdentity: commit: %w", err)
	}
	out.CarriedName = carriedName
	out.summarize()
	return out, nil
}
