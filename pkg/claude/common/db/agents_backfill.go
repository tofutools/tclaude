package db

import (
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// backfillAgents seeds `agents` + `agent_conversations` from the existing
// conv-keyed identity state, so the new actor layer reflects the current DB
// the moment the migration lands. Split out from the migration so it is
// independently testable, and written to operate on the migration's *sql.DB
// (never re-entering Open(), which is mid-init while migrate() runs).
//
// The model, validated against the live code (and with the Codex agent that
// authored the JOH-26 rewrite):
//
//   - Collapse REPLACEMENT chains into ONE actor. The signal is strictly the
//     presence of an `agent_conv_succession` edge — NOT the reason string.
//     reincarnate and Claude Code's /clear both record such an edge; a
//     conversation reachable along succession edges is the same actor.
//   - Do NOT collapse along clone history. A clone is a fork: it records no
//     succession edge and add-copies identity, so it naturally falls out as
//     its own actor with its own conv.
//   - Each chain's actor-level facts (created_via, pending_name, retire
//     state) are taken from its HEAD generation's enrollment — the head is
//     the live actor; a predecessor's retired_at merely marks it superseded,
//     never the actor's retirement.
//
// Idempotent: agent allocation is guarded on the head not already being
// linked, and every conv link is INSERT OR IGNORE, so a re-run converges on
// the same mapping without minting duplicate actors.
func backfillAgents(d *sql.DB) error {
	now := time.Now()

	// 1. Load the succession forest: old_conv_id -> new_conv_id. Each conv
	// has at most one successor (old_conv_id is the table PK), so walking the
	// map forward from any node reaches that chain's head.
	succ, err := loadSuccessionMap(d)
	if err != nil {
		return err
	}

	// 2. Every conv that should belong to an actor: the union of all
	// identity-table references plus the enrollment roster. Mirrors
	// backfillAgentEnrollment's reach so no agent is missed.
	convs, err := collectAgentConvs(d)
	if err != nil {
		return err
	}

	// 3. For each candidate, resolve its chain head and ensure that head has
	// an actor, then link the candidate to it. headToAgent makes the whole
	// pass coherent within one run: every conv sharing a head shares its
	// actor.
	headToAgent := make(map[string]string, len(convs))
	for _, conv := range convs {
		head := resolveHeadTx(conv, succ)
		agentID := headToAgent[head]
		if agentID == "" {
			agentID, err = ensureAgentForHeadTx(d, head, now)
			if err != nil {
				return err
			}
			headToAgent[head] = agentID
		}
		role := ConvRoleGeneration
		reason := "backfill"
		if conv == head {
			role = ConvRoleHead
		}
		// Per-conv link is non-fatal: linkConvTx is conflict-aware and errors
		// on a cross-actor relink, which can only arise from a CORRUPTED
		// succession cycle (real succession data is a forest — every
		// new_conv_id is freshly minted — so chain heads are consistent and
		// this never fires). Degrade gracefully on such corruption: log and
		// skip the offending conv rather than failing the whole v72 migration
		// and wedging DB-open. A genuine DB error surfaces the same way — the
		// conv is simply not backfilled and re-syncs on its next enroll.
		if err := linkConvTx(d, conv, agentID, role, reason, now); err != nil {
			slog.Warn("backfillAgents: skipping conv with inconsistent actor mapping",
				"conv", conv, "head", head, "agent", agentID, "error", err)
		}
	}
	return nil
}

// loadSuccessionMap reads agent_conv_succession into an old->new map. Returns
// an empty map when the table is absent — the partial-schema heal tests seed
// only a subset of tables, and a real DB always has it (created at v15) by the
// time v72 runs.
func loadSuccessionMap(d *sql.DB) (map[string]string, error) {
	if ok, err := tableExists(d, "agent_conv_succession"); err != nil || !ok {
		return map[string]string{}, err
	}
	rows, err := d.Query(`SELECT old_conv_id, new_conv_id FROM agent_conv_succession`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	succ := map[string]string{}
	for rows.Next() {
		var old, nw string
		if err := rows.Scan(&old, &nw); err != nil {
			return nil, err
		}
		succ[old] = nw
	}
	return succ, rows.Err()
}

// resolveHeadTx walks the succession map forward from conv to the chain head
// (a conv with no successor). Runs against an in-memory map so it never
// re-enters Open(). A `seen` set breaks on the first revisited node, so a
// malformed cycle terminates deterministically instead of looping or
// mis-resolving on a return-to-origin-only check. Real succession data is a
// forest (every new_conv_id is freshly minted), so cycles never arise — this
// is purely a corruption guard.
func resolveHeadTx(conv string, succ map[string]string) string {
	seen := map[string]bool{conv: true}
	cur := conv
	for {
		next, ok := succ[cur]
		if !ok || seen[next] {
			return cur
		}
		seen[next] = true
		cur = next
	}
}

// ensureAgentForHeadTx returns the actor owning head, allocating one (with
// actor facts carried from head's enrollment) when head is not yet linked.
// It links head itself so a later lookup in the same run resolves it.
func ensureAgentForHeadTx(d *sql.DB, head string, now time.Time) (string, error) {
	if existing, err := agentIDForConvTx(d, head); err != nil {
		return "", err
	} else if existing != "" {
		return existing, nil
	}
	// Idempotency against a crash between the agents INSERT and the head
	// link below (schema_version would still read pre-migration, so the
	// whole pass re-runs): an agents row may already carry head as its
	// current_conv_id without the link existing yet. Reuse it rather than
	// minting a second actor and colliding on the current_conv_id UNIQUE.
	var orphan string
	switch err := d.QueryRow(`SELECT agent_id FROM agents WHERE current_conv_id = ?`, head).Scan(&orphan); {
	case err == nil && orphan != "":
		if linkErr := linkConvTx(d, head, orphan, ConvRoleHead, "backfill", now); linkErr != nil {
			return "", linkErr
		}
		return orphan, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", err
	}

	created, via, retiredAt, retiredBy, retireReason, pendingName := headEnrollmentFacts(d, head, now)
	agentID := newAgentID()
	if _, err := d.Exec(`INSERT INTO agents
		(agent_id, current_conv_id, created_at, created_via,
		 retired_at, retired_by, retire_reason, pending_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, head, created, via, retiredAt, retiredBy, retireReason, pendingName); err != nil {
		return "", err
	}
	if err := linkConvTx(d, head, agentID, ConvRoleHead, "backfill", now); err != nil {
		return "", err
	}
	return agentID, nil
}

// headEnrollmentFacts reads the actor-level facts to seed an agent from its
// head generation's agent_enrollment row. Falls back to sensible defaults
// (created_at = now, created_via = "backfill") when the head has no
// enrollment row, or when agent_enrollment is absent entirely (a
// partial-schema heal DB) — defensive: a conv referenced only by an identity
// table still becomes an actor.
func headEnrollmentFacts(d *sql.DB, head string, now time.Time) (createdAt, via, retiredAt, retiredBy, retireReason, pendingName string) {
	via = "backfill"
	createdAt = now.Format(time.RFC3339Nano)
	if ok, err := tableExists(d, "agent_enrollment"); err != nil || !ok {
		return createdAt, via, "", "", "", ""
	}
	row := d.QueryRow(`SELECT enrolled_at, enrolled_via,
		retired_at, retired_by, retire_reason, pending_name
		FROM agent_enrollment WHERE conv_id = ?`, head)
	var ea, ev, ra, rb, rr, pn string
	if err := row.Scan(&ea, &ev, &ra, &rb, &rr, &pn); err != nil {
		return createdAt, via, "", "", "", ""
	}
	if ea != "" {
		createdAt = ea
	}
	if ev != "" {
		via = ev
	}
	return createdAt, via, ra, rb, rr, pn
}

// collectAgentConvs returns the distinct set of conv-ids referenced by any
// agentic table — the same reach as backfillAgentEnrollment, plus the
// enrollment roster itself and both ends of every succession edge, so every
// actor and every past generation is covered.
//
// The UNION is assembled only from tables that actually exist: the
// partial-schema heal tests seed an arbitrary subset, and a real DB has all
// of these by the time v72 runs. A missing table is skipped, not an error.
func collectAgentConvs(d *sql.DB) ([]string, error) {
	sources := []struct{ table, col string }{
		{"agent_enrollment", "conv_id"},
		{"agent_group_members", "conv_id"},
		{"agent_group_owners", "conv_id"},
		{"agent_permissions", "conv_id"},
		{"agent_sudo_grants", "conv_id"},
		{"agent_notify_prefs", "conv_id"},
		{"agent_head_aliases", "anchor_conv_id"},
		{"agent_conv_succession", "old_conv_id"},
		{"agent_conv_succession", "new_conv_id"},
		{"agent_clone_history", "source_conv_id"},
		{"agent_spawn_history", "spawner_conv_id"},
		{"agent_cron_jobs", "owner_conv"},
		{"agent_cron_jobs", "target_conv"},
	}
	var selects []string
	for _, s := range sources {
		// Skip a source whose table or column is absent. The column guard
		// matters AFTER the v73/v74 cutovers: the membership/owner/permission/
		// sudo/notify tables (v73) and the cron owner/target + spawn/clone
		// history (v74) become agent-keyed (no conv_id / *_conv column), and
		// their actors already exist by construction, so they contribute
		// nothing to a coverage backfill. Pre-cutover they still carry the conv
		// column and are included.
		ok, err := columnExists(d, s.table, s.col)
		if err != nil {
			return nil, err
		}
		if ok {
			selects = append(selects, "SELECT "+s.col+" AS conv_id FROM "+s.table)
		}
	}
	if len(selects) == 0 {
		return nil, nil
	}
	rows, err := d.Query(strings.Join(selects, " UNION "))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		if c != "" {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// tableExists reports whether a table of the given name is present.
func tableExists(d *sql.DB, name string) (bool, error) {
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// columnExists reports whether `table` exists AND has a column named `col`.
// pragma_table_info returns no rows for a missing table, so a single query
// answers both. Used by the coverage backfill to tolerate the v73 cutover,
// after which some source tables lose their conv_id column.
func columnExists(d *sql.DB, table, col string) (bool, error) {
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
