package db

import (
	"fmt"
	"time"
)

// AgentIdentityMigration is the outcome of MigrateAgentIdentity — the
// per-table count of rows rekeyed old → new, plus the agent's carried
// display name. Items is a compact human-readable summary (only the
// non-zero facets) for API responses and logs.
type AgentIdentityMigration struct {
	GroupMembers int64  // agent_group_members rows rekeyed
	Ownerships   int64  // agent_group_owners rows rekeyed
	Permissions  int64  // agent_permissions rows rekeyed
	CronJobs     int64  // agent_cron_jobs rows whose owner/target ref moved
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

// MigrateAgentIdentity rekeys every conv-id-keyed agentd identity row
// from oldConv onto newConv, in a SINGLE SQLite transaction:
//
//   - agent_group_members  — group memberships
//   - agent_group_owners   — group ownerships
//   - agent_permissions    — per-conv permission overrides (grant AND deny)
//   - agent_cron_jobs      — owner/target conv refs
//
// Within the same transaction it records the succession edge
// (agent_conv_succession: old → new, so stale references resolve
// forward via db.ResolveLatestConv), enrolls newConv as an agent,
// carries the agent's display name onto newConv
// (agent_enrollment.pending_name), and drops oldConv's enrollment row
// (the old conv is superseded — its identity now lives on newConv;
// without the drop it lingers on the agent roster as an offline ghost).
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
// granter is the audit string written to the migrated permission /
// ownership rows' granted_by column (e.g. "system:reincarnate",
// "system:clear") — same convention clone / groups-create follow.
// Group-membership rows are rekeyed pure (conv_id only): role, descr
// and the original joined_at are preserved.
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

	nowNano := time.Now().Format(time.RFC3339Nano)
	nowSec := time.Now().UTC().Format(time.RFC3339)

	// --- rekey conv-id-keyed identity rows old → new ---
	//
	// `UPDATE OR REPLACE` so a (theoretical) pre-existing newConv row —
	// newConv is a fresh conv-id in practice, so this never triggers —
	// is resolved in favour of the migrated identity rather than
	// aborting the whole transaction on the unique constraint.
	//
	// Memberships rekey pure (conv_id only) — joined_at / role / descr
	// ride along untouched. Permission + ownership rows additionally
	// re-stamp granted_by/granted_at, matching the audit convention the
	// reincarnate / clone paths already use for daemon-performed grants.
	memRes, err := tx.Exec(
		`UPDATE OR REPLACE agent_group_members SET conv_id = ? WHERE conv_id = ?`,
		newConv, oldConv)
	if err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: rekey memberships: %w", err)
	}
	out.GroupMembers, _ = memRes.RowsAffected()

	ownRes, err := tx.Exec(
		`UPDATE OR REPLACE agent_group_owners
		    SET conv_id = ?, granted_by = ?, granted_at = ? WHERE conv_id = ?`,
		newConv, granter, nowNano, oldConv)
	if err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: rekey ownerships: %w", err)
	}
	out.Ownerships, _ = ownRes.RowsAffected()

	permRes, err := tx.Exec(
		`UPDATE OR REPLACE agent_permissions
		    SET conv_id = ?, granted_by = ?, granted_at = ? WHERE conv_id = ?`,
		newConv, granter, nowNano, oldConv)
	if err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: rekey permissions: %w", err)
	}
	out.Permissions, _ = permRes.RowsAffected()

	// cron jobs: an agent can be a job's owner, its target, or both —
	// rewrite whichever side(s) reference oldConv. Mirrors
	// db.MigrateCronJobConvRef (kept standalone for non-transactional
	// callers).
	cronRes, err := tx.Exec(`UPDATE agent_cron_jobs
		SET owner_conv  = CASE WHEN owner_conv  = ?1 THEN ?2 ELSE owner_conv END,
		    target_conv = CASE WHEN target_conv = ?1 THEN ?2 ELSE target_conv END
		WHERE owner_conv = ?1 OR target_conv = ?1`, oldConv, newConv)
	if err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: rekey cron jobs: %w", err)
	}
	out.CronJobs, _ = cronRes.RowsAffected()

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

	// --- drop the superseded old conv's enrollment ---
	// Its identity has moved to newConv; leaving the row makes oldConv
	// linger on the agent roster as an offline ghost.
	if _, err := tx.Exec(`DELETE FROM agent_enrollment WHERE conv_id = ?`, oldConv); err != nil {
		return out, fmt.Errorf("MigrateAgentIdentity: drop old enrollment: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AgentIdentityMigration{}, fmt.Errorf("MigrateAgentIdentity: commit: %w", err)
	}
	out.CarriedName = carriedName
	out.summarize()
	return out, nil
}
