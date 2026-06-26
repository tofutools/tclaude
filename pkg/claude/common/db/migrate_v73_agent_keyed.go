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
// agent_id row, which is exactly right.
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

	// Guarantee every conv referenced by the identity tables maps to an actor,
	// so the conv_id → agent_id join below never drops a row. Idempotent.
	if err := backfillAgents(db); err != nil {
		return fmt.Errorf("migrate v72→v73 (coverage backfill): %w", err)
	}

	// agent_conversations is the join spine; absent only on a partial-schema
	// heal DB, where there is nothing to cut over.
	if ok, err := tableExists(db, "agent_conversations"); err != nil {
		return fmt.Errorf("migrate v72→v73 (probe agent_conversations): %w", err)
	} else if !ok {
		if _, err := db.Exec(`UPDATE schema_version SET version = 73`); err != nil {
			return fmt.Errorf("migrate v72→v73 (version, no-op): %w", err)
		}
		return nil
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
			insert: `INSERT OR IGNORE INTO agent_group_members_v73
				(group_id, agent_id, role, descr, joined_at)
				SELECT m.group_id, ac.agent_id, m.role, m.descr, m.joined_at
				FROM agent_group_members m
				JOIN agent_conversations ac ON ac.conv_id = m.conv_id`,
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
			insert: `INSERT OR IGNORE INTO agent_group_owners_v73
				(group_id, agent_id, granted_at, granted_by)
				SELECT o.group_id, ac.agent_id, o.granted_at, o.granted_by
				FROM agent_group_owners o
				JOIN agent_conversations ac ON ac.conv_id = o.conv_id`,
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
			insert: `INSERT OR IGNORE INTO agent_permissions_v73
				(agent_id, slug, granted_at, granted_by, effect)
				SELECT ac.agent_id, p.slug, p.granted_at, p.granted_by, p.effect
				FROM agent_permissions p
				JOIN agent_conversations ac ON ac.conv_id = p.conv_id`,
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
			insert: `INSERT OR IGNORE INTO agent_notify_prefs_v73
				(agent_id, mode, updated_at)
				SELECT ac.agent_id, n.mode, n.updated_at
				FROM agent_notify_prefs n
				JOIN agent_conversations ac ON ac.conv_id = n.conv_id`,
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

// vacuumBackup writes a consistent snapshot of the live DB to <dbpath><suffix>
// via VACUUM INTO before a destructive migration. Best-effort: any failure is
// logged and the migration proceeds (the operator's own backups remain the
// ultimate safety net; this is convenience insurance). No-op when the DB path
// is unknown (e.g. some test harnesses).
func vacuumBackup(db *sql.DB, suffix string) {
	path := DBPath()
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
