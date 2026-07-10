package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// migrateV72toV73 cuts the authorization / identity tables over from being
// keyed on the (rotating) harness conv_id to the stable agent_id introduced in
// v72 (JOH-26). After this, a reincarnate / Claude Code /clear no longer needs
// to physically rekey these rows — the actor's agent_id never moves, so its
// memberships, ownerships, permissions, sudo grants and notify pref simply stay
// put while only the conv pointer advances.
//
// Scope (the authz/identity-critical set): agent_group_members,
// agent_group_owners, agent_permissions, agent_sudo_grants, agent_notify_prefs.
// agent_cron_jobs and the spawn/clone rate-limit history stay conv-keyed for now
// (a later stage) — MigrateAgentIdentity keeps rekeying just those.
//
// Each table is rebuilt: a new agent_id-keyed table is filled by joining the old
// conv-keyed rows through agent_conversations (conv_id → agent_id), then swapped
// in. INSERT OR IGNORE collapses the (rare) case where two generations of the
// same actor each carried a row for the same (group/slug) — they map to one
// agent_id row, which is exactly right. The collapse is DETERMINISTIC: each
// insert's ORDER BY puts the row that should survive first (permissions: DENY
// over grant, then newest; the others: newest), so the outcome never depends on
// scan order. (In a normally-migrated DB no collision arises — the pre-cutover
// rotation kept each actor's rows on a single conv — so this is belt-and-braces.)
//
// Safety:
//   - A VACUUM INTO snapshot is written before any destructive change, so the
//     pre-cutover DB is recoverable (best-effort: logged, never fatal).
//   - backfillAgents is re-run first so every conv referenced by these tables is
//     guaranteed to have an agent_conversations row — no row is dropped by the
//     join.
//   - The rebuilds run in ONE transaction (SQLite DDL is transactional), so a
//     failure rolls the whole cutover back and the version stays at 72; the
//     migration then re-runs cleanly. Each rebuild is guarded on its source
//     table (and agent_conversations) existing, so a partial-schema heal DB
//     advances without tripping on a missing table.
func migrateV72toV73(db *sql.DB) error {
	// Pre-cutover snapshot (best-effort).
	vacuumBackup(db, ".pre-v73-agentkey.bak")

	// agent_conversations is the join spine. Probe it FIRST: on a partial-schema
	// heal DB that lacks it there is nothing to cut over, and running the
	// coverage backfill (which INSERTs into agents/agent_conversations) would
	// just error on the missing tables. Normally v72 created+populated both
	// immediately before this runs, so the probe passes and we proceed.
	if ok, err := tableExists(db, "agent_conversations"); err != nil {
		return fmt.Errorf("migrate v72→v73 (probe agent_conversations): %w", err)
	} else if !ok {
		if _, err := db.Exec(`UPDATE schema_version SET version = 73`); err != nil {
			return fmt.Errorf("migrate v72→v73 (version, no-op): %w", err)
		}
		return nil
	}

	// Guarantee every conv referenced by the identity tables maps to an actor,
	// so the conv_id → agent_id join below never drops a row. Idempotent.
	if err := backfillAgents(db); err != nil {
		return fmt.Errorf("migrate v72→v73 (coverage backfill): %w", err)
	}

	// Strict coverage gate. The rebuilds below INNER JOIN each source table to
	// agent_conversations and then DROP the originals, so any identity row whose
	// conv has no actor mapping would be silently lost. backfillAgents above is
	// meant to guarantee full coverage, but it degrades gracefully (logs + skips)
	// on a corrupted succession mapping rather than wedging DB-open — a lenient
	// posture that is right for the additive v72 backfill but wrong here, at the
	// destructive cutover. So before touching anything we refuse to proceed if a
	// single identity row would be dropped: the version stays at 72, the
	// .pre-v73-agentkey.bak snapshot is intact, and the operator can investigate
	// rather than discover missing authz state after the fact.
	if unmapped, err := unmappedIdentityRows(db); err != nil {
		return fmt.Errorf("migrate v72→v73 (coverage check): %w", err)
	} else if len(unmapped) > 0 {
		return fmt.Errorf("migrate v72→v73: refusing to cut over — identity rows have no agent mapping "+
			"after backfill (would be dropped by the rebuild): %v; the pre-cutover DB is unchanged "+
			"(snapshot at <db>.pre-v73-agentkey.bak)", unmapped)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v72→v73: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rebuilds := []agentKeyRebuild{
		{
			table: "agent_group_members",
			create: `CREATE TABLE agent_group_members_v73 (
				group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
				agent_id  TEXT NOT NULL,
				role      TEXT NOT NULL DEFAULT '',
				descr     TEXT NOT NULL DEFAULT '',
				joined_at TEXT NOT NULL,
				PRIMARY KEY (group_id, agent_id)
			)`,
			// Newest generation's role/descr wins a same-(group,agent) collapse.
			insert: `INSERT OR IGNORE INTO agent_group_members_v73
				(group_id, agent_id, role, descr, joined_at)
				SELECT m.group_id, ac.agent_id, m.role, m.descr, m.joined_at
				FROM agent_group_members m
				JOIN agent_conversations ac ON ac.conv_id = m.conv_id
				ORDER BY m.joined_at DESC`,
			indexes: []string{`CREATE INDEX IF NOT EXISTS idx_agent_group_members_agent
				ON agent_group_members(agent_id)`},
		},
		{
			table: "agent_group_owners",
			create: `CREATE TABLE agent_group_owners_v73 (
				group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
				agent_id   TEXT NOT NULL,
				granted_at TEXT NOT NULL,
				granted_by TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (group_id, agent_id)
			)`,
			// Newest grant wins a same-(group,agent) collapse.
			insert: `INSERT OR IGNORE INTO agent_group_owners_v73
				(group_id, agent_id, granted_at, granted_by)
				SELECT o.group_id, ac.agent_id, o.granted_at, o.granted_by
				FROM agent_group_owners o
				JOIN agent_conversations ac ON ac.conv_id = o.conv_id
				ORDER BY o.granted_at DESC`,
			indexes: []string{`CREATE INDEX IF NOT EXISTS idx_agent_group_owners_agent
				ON agent_group_owners(agent_id)`},
		},
		{
			table: "agent_permissions",
			create: `CREATE TABLE agent_permissions_v73 (
				agent_id   TEXT NOT NULL,
				slug       TEXT NOT NULL,
				granted_at TEXT NOT NULL,
				granted_by TEXT NOT NULL DEFAULT '',
				effect     TEXT NOT NULL DEFAULT 'grant' CHECK (effect IN ('grant', 'deny')),
				PRIMARY KEY (agent_id, slug)
			)`,
			// Security-relevant collapse: if two generations carried conflicting
			// overrides for the same (agent, slug), DENY must win (it
			// unconditionally overrides a grant), then the newest. ORDER BY puts
			// the surviving row first so INSERT OR IGNORE keeps it.
			insert: `INSERT OR IGNORE INTO agent_permissions_v73
				(agent_id, slug, granted_at, granted_by, effect)
				SELECT ac.agent_id, p.slug, p.granted_at, p.granted_by, p.effect
				FROM agent_permissions p
				JOIN agent_conversations ac ON ac.conv_id = p.conv_id
				ORDER BY CASE WHEN p.effect = 'deny' THEN 0 ELSE 1 END, p.granted_at DESC`,
			indexes: []string{`CREATE INDEX IF NOT EXISTS idx_agent_permissions_slug
				ON agent_permissions(slug)`},
		},
		{
			table: "agent_sudo_grants",
			create: `CREATE TABLE agent_sudo_grants_v73 (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				agent_id    TEXT NOT NULL,
				slug        TEXT NOT NULL,
				granted_at  TEXT NOT NULL,
				expires_at  TEXT NOT NULL,
				granted_by  TEXT NOT NULL,
				reason      TEXT NOT NULL DEFAULT '',
				revoked_at  TEXT NOT NULL DEFAULT ''
			)`,
			// Preserve the grant ids (referenced in audit strings) — each grant
			// is its own row, so no collapse; INSERT OR IGNORE only guards the
			// (impossible) id collision.
			insert: `INSERT OR IGNORE INTO agent_sudo_grants_v73
				(id, agent_id, slug, granted_at, expires_at, granted_by, reason, revoked_at)
				SELECT s.id, ac.agent_id, s.slug, s.granted_at, s.expires_at, s.granted_by, s.reason, s.revoked_at
				FROM agent_sudo_grants s
				JOIN agent_conversations ac ON ac.conv_id = s.conv_id`,
			indexes: []string{`CREATE INDEX IF NOT EXISTS idx_sudo_active
				ON agent_sudo_grants(agent_id, expires_at) WHERE revoked_at = ''`},
		},
		{
			table: "agent_notify_prefs",
			create: `CREATE TABLE agent_notify_prefs_v73 (
				agent_id   TEXT PRIMARY KEY,
				mode       TEXT NOT NULL CHECK (mode IN ('on', 'off')),
				updated_at TEXT NOT NULL
			)`,
			// Newest pref wins a same-agent collapse.
			insert: `INSERT OR IGNORE INTO agent_notify_prefs_v73
				(agent_id, mode, updated_at)
				SELECT ac.agent_id, n.mode, n.updated_at
				FROM agent_notify_prefs n
				JOIN agent_conversations ac ON ac.conv_id = n.conv_id
				ORDER BY n.updated_at DESC`,
		},
	}

	for _, rb := range rebuilds {
		if err := rb.run(tx); err != nil {
			return fmt.Errorf("migrate v72→v73 (%s): %w", rb.table, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 73`); err != nil {
		return fmt.Errorf("migrate v72→v73 (version): %w", err)
	}
	return tx.Commit()
}

// agentKeyRebuild describes a single conv_id → agent_id table rebuild.
type agentKeyRebuild struct {
	table   string
	create  string   // CREATE TABLE <table>_v73 (...)
	insert  string   // INSERT ... SELECT ... JOIN agent_conversations
	indexes []string // recreated after the rename
}

// run performs one rebuild inside tx: create the agent-keyed shadow table,
// backfill it through agent_conversations, drop the old conv-keyed table, swap
// the shadow into place, and recreate its indexes. Guarded on the source table
// existing, so a partial-schema heal DB skips it.
func (rb agentKeyRebuild) run(tx *sql.Tx) error {
	var have int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, rb.table,
	).Scan(&have); err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	if have == 0 {
		return nil // partial-schema heal DB — nothing to cut over
	}
	if _, err := tx.Exec(rb.create); err != nil {
		return fmt.Errorf("create shadow: %w", err)
	}
	if _, err := tx.Exec(rb.insert); err != nil {
		return fmt.Errorf("backfill shadow: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE ` + rb.table); err != nil {
		return fmt.Errorf("drop old: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE ` + rb.table + `_v73 RENAME TO ` + rb.table); err != nil {
		return fmt.Errorf("rename shadow: %w", err)
	}
	for _, idx := range rb.indexes {
		if _, err := tx.Exec(idx); err != nil {
			return fmt.Errorf("recreate index: %w", err)
		}
	}
	return nil
}

// unmappedIdentityRows reports, per source identity table, how many rows
// reference a conv that has no agent_conversations mapping — i.e. rows the
// destructive conv_id → agent_id rebuild would silently drop. An empty map
// means full coverage (safe to cut over). Column-aware: a table that is absent
// or already agent-keyed (no conv_id column) contributes nothing, so the check
// is a no-op on a re-run or a partial-schema heal DB.
func unmappedIdentityRows(d *sql.DB) (map[string]int, error) {
	out := map[string]int{}
	for _, table := range []string{
		"agent_group_members",
		"agent_group_owners",
		"agent_permissions",
		"agent_sudo_grants",
		"agent_notify_prefs",
	} {
		ok, err := columnExists(d, table, "conv_id")
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // table missing or already agent-keyed — nothing to map
		}
		// conv_id is NOT NULL in every source table and is the PK of
		// agent_conversations, so NOT IN carries no NULL-semantics surprise.
		// The table name comes from the hardcoded list above, never user input.
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM ` + table +
			` WHERE conv_id NOT IN (SELECT conv_id FROM agent_conversations)`).Scan(&n); err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
		if n > 0 {
			out[table] = n
		}
	}
	return out, nil
}

// vacuumBackup writes a consistent snapshot of the live DB to <dbpath><suffix>
// via VACUUM INTO before a destructive migration. Best-effort: any failure is
// logged and the migration proceeds (the operator's own backups remain the
// ultimate safety net; this is convenience insurance). No-op when the DB path
// is unknown (e.g. some test harnesses).
func vacuumBackup(db *sql.DB, suffix string) {
	path := globalDBPath
	if path == "" {
		path = DBPath()
	}
	if path == "" {
		return
	}
	bak := path + suffix
	_ = os.Remove(bak) // VACUUM INTO fails if the target already exists
	// The path is tclaude-controlled (DBPath under the home dir), not user
	// input; single-quote-escape defensively all the same.
	stmt := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(bak, "'", "''"))
	if _, err := db.Exec(stmt); err != nil {
		slog.Warn("pre-migration backup failed; proceeding without a snapshot",
			"path", bak, "error", err)
		return
	}
	slog.Info("pre-migration backup written", "path", bak, "at", time.Now().Format(time.RFC3339))
}
