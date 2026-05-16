package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const currentVersion = 35

func migrate(db *sql.DB) error {
	ver := schemaVersion(db)
	if ver == currentVersion {
		return nil
	}

	if ver == 0 {
		if err := createSchema(db); err != nil {
			return err
		}
		if err := importLegacyData(db); err != nil {
			return err
		}
		ver = 1 // createSchema sets version to 1
	}

	if ver < 2 {
		if err := migrateV1toV2(db); err != nil {
			return err
		}
	}

	if ver < 3 {
		if err := migrateV2toV3(db); err != nil {
			return err
		}
	}

	if ver < 4 {
		if err := migrateV3toV4(db); err != nil {
			return err
		}
	}

	if ver < 5 {
		if err := migrateV4toV5(db); err != nil {
			return err
		}
	}

	if ver < 6 {
		if err := migrateV5toV6(db); err != nil {
			return err
		}
	}

	if ver < 7 {
		if err := migrateV6toV7(db); err != nil {
			return err
		}
	}

	if ver < 8 {
		if err := migrateV7toV8(db); err != nil {
			return err
		}
	}

	if ver < 9 {
		if err := migrateV8toV9(db); err != nil {
			return err
		}
	}

	if ver < 10 {
		if err := migrateV9toV10(db); err != nil {
			return err
		}
	}

	if ver < 11 {
		if err := migrateV10toV11(db); err != nil {
			return err
		}
	}

	if ver < 12 {
		if err := migrateV11toV12(db); err != nil {
			return err
		}
	}

	if ver < 13 {
		if err := migrateV12toV13(db); err != nil {
			return err
		}
	}

	if ver < 14 {
		if err := migrateV13toV14(db); err != nil {
			return err
		}
	}

	if ver < 15 {
		if err := migrateV14toV15(db); err != nil {
			return err
		}
	}

	if ver < 16 {
		if err := migrateV15toV16(db); err != nil {
			return err
		}
	}

	if ver < 17 {
		if err := migrateV16toV17(db); err != nil {
			return err
		}
	}

	if ver < 18 {
		if err := migrateV17toV18(db); err != nil {
			return err
		}
	}

	if ver < 19 {
		if err := migrateV18toV19(db); err != nil {
			return err
		}
	}

	if ver < 20 {
		if err := migrateV19toV20(db); err != nil {
			return err
		}
	}

	if ver < 21 {
		if err := migrateV20toV21(db); err != nil {
			return err
		}
	}

	if ver < 22 {
		if err := migrateV21toV22(db); err != nil {
			return err
		}
	}

	if ver < 23 {
		if err := migrateV22toV23(db); err != nil {
			return err
		}
	}

	if ver < 24 {
		if err := migrateV23toV24(db); err != nil {
			return err
		}
	}

	if ver < 25 {
		if err := migrateV24toV25(db); err != nil {
			return err
		}
	}

	if ver < 26 {
		if err := migrateV25toV26(db); err != nil {
			return err
		}
	}

	if ver < 27 {
		if err := migrateV26toV27(db); err != nil {
			return err
		}
	}

	if ver < 28 {
		if err := migrateV27toV28(db); err != nil {
			return err
		}
	}

	if ver < 29 {
		if err := migrateV28toV29(db); err != nil {
			return err
		}
	}

	if ver < 30 {
		if err := migrateV29toV30(db); err != nil {
			return err
		}
	}

	if ver < 31 {
		if err := migrateV30toV31(db); err != nil {
			return err
		}
	}

	if ver < 32 {
		if err := migrateV31toV32(db); err != nil {
			return err
		}
	}

	if ver < 33 {
		if err := migrateV32toV33(db); err != nil {
			return err
		}
	}

	if ver < 34 {
		if err := migrateV33toV34(db); err != nil {
			return err
		}
	}

	if ver < 35 {
		if err := migrateV34toV35(db); err != nil {
			return err
		}
	}

	return nil
}

// migrateV33toV34 adds agent_spawn_history — an append-only audit of
// every agent-initiated `tclaude agent spawn` that passed the spawn
// rate-limit check. The daemon's spawn handler does an atomic
// INSERT-WHERE-(count < max) against this table to cap how many agents
// a single caller-agent can spawn per rolling window (see
// db.ClaimSpawnSlot), so a spawn-capable agent stuck in a loop can't
// fork CC instances unboundedly.
//
// Sibling of agent_clone_history (v18→v19): same append-only shape,
// but keyed on the *spawner* (the caller) rather than a source conv,
// and counted (N per window) rather than gated (1 per cooldown) —
// `tclaude agent spawn` always creates a brand-new conv, so there is
// no "same source" to deduplicate against.
//
// Shape:
//   - spawner_conv_id is the caller-agent that requested the spawn.
//     Human-initiated spawns are never recorded (humans bypass the
//     rate limit), so the column is always a real conv-id.
//   - spawned_at is the wall-clock instant the slot was claimed
//     (RFC3339Nano so closely-spaced attempts compare correctly).
//
// Append-only by construction: rows are never updated or deleted in
// the production code path. A future cleanup verb could prune rows
// older than the widest window if the table grows uncomfortably.
func migrateV33toV34(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_spawn_history (
			spawner_conv_id TEXT NOT NULL,
			spawned_at      TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_spawn_history_spawner
			ON agent_spawn_history(spawner_conv_id, spawned_at);

		UPDATE schema_version SET version = 34;
	`)
	if err != nil {
		return fmt.Errorf("migrate v33→v34: %w", err)
	}
	return nil
}

// migrateV32toV33 adds agent_groups.max_members — an optional hard cap
// on how many members a group may hold. A `tclaude agent spawn` that
// would push the group over the cap is refused (the spawn-guardrail
// layer). 0 means unlimited (the pre-feature behaviour), so existing
// rows keep their prior unbounded semantics on upgrade. A human raises
// the cap to add more; it is a property of the group, not of any
// caller.
func migrateV32toV33(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_groups
			ADD COLUMN max_members INTEGER NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 33;
	`)
	if err != nil {
		return fmt.Errorf("migrate v32→v33: %w", err)
	}
	return nil
}

// migrateV34toV35 drops the now-defunct agent_group_members.alias
// column. An agent has exactly ONE name — its conversation title
// (conv_index.custom_title). The per-group alias was pure duplication:
// spawn always set it equal to the title (the daemon injected
// `/rename <alias>`), and per-group semantics are already carried by
// the member role/descr fields. See docs/plans/DONE/scrap-agent-alias.md.
//
// A plain ALTER TABLE ... DROP COLUMN suffices: the column is a bare
// TEXT field — not part of the (group_id, conv_id) primary key, not
// indexed (idx_agent_group_members_conv covers conv_id only), and not
// referenced by any foreign key, generated column or CHECK constraint.
// SQLite has supported DROP COLUMN since 3.35 and the bundled
// modernc.org/sqlite is well past that, so no table-rebuild dance is
// needed.
func migrateV34toV35(db *sql.DB) error {
	if _, err := db.Exec(`
		ALTER TABLE agent_group_members DROP COLUMN alias;

		UPDATE schema_version SET version = 35;
	`); err != nil {
		return fmt.Errorf("migrate v34→v35 (drop agent_group_members.alias): %w", err)
	}
	return nil
}

// migrateV31toV32 adds conv_index.git_branch_startup — the git branch
// the conversation's FIRST turn was stamped with, i.e. the branch
// Claude Code was launched on. The existing git_branch column is
// last-wins (it tracks the current branch as the session moves);
// git_branch_startup is first-wins and immutable, so a dashboard can
// honestly show an "init → now" branch pair. Existing rows backfill
// to ” and self-heal on the next .jsonl rescan (the scanner fills
// the column, and UpsertConvIndex carries it through ON CONFLICT).
func migrateV31toV32(db *sql.DB) error {
	if _, err := db.Exec(`
		ALTER TABLE conv_index
			ADD COLUMN git_branch_startup TEXT NOT NULL DEFAULT '';
	`); err != nil {
		return fmt.Errorf("migrate v31→v32 (add git_branch_startup): %w", err)
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = 32;`); err != nil {
		return fmt.Errorf("migrate v31→v32 (version): %w", err)
	}
	return nil
}

// migrateV30toV31 cleans up ghost agent enrollments for superseded
// conversations. The v29→v30 backfill drew conv-ids from
// agent_conv_succession's old_conv_id column, so every reincarnation
// predecessor got enrolled as an active agent — even though its
// identity has long since moved to the chain head. Those ghosts
// cluttered the agent roster, and worse: every promote/retire verb
// aimed at a predecessor redirects forward through the succession
// chain (ResolveSelector → ResolveLatestConv), so retiring a ghost
// actually hit the chain head and failed once the head was retired.
//
// The live reincarnate path already deletes the predecessor's
// enrollment (reincarnate.go). This migration retroactively applies
// the same rule to the chains the backfill mis-enrolled. The fixed
// backfillAgentEnrollment no longer creates these rows, so on a fresh
// v29→v30→v31 run this delete is a harmless no-op.
func migrateV30toV31(db *sql.DB) error {
	if _, err := db.Exec(`
		DELETE FROM agent_enrollment
		WHERE conv_id IN (
			SELECT old_conv_id FROM agent_conv_succession
			WHERE old_conv_id IS NOT NULL AND old_conv_id != ''
		);
	`); err != nil {
		return fmt.Errorf("migrate v30→v31 (delete superseded enrollments): %w", err)
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = 31;`); err != nil {
		return fmt.Errorf("migrate v30→v31 (version): %w", err)
	}
	return nil
}

// migrateV29toV30 adds agent_enrollment — the explicit "is this conv an
// agent" record. Before this, agent-ness was a read-time heuristic
// (group member ∨ grant holder ∨ live tmux); an ungrouped conv whose
// tmux had died was invisible on every agent surface, and there was no
// way to demote an agent without deleting its conversation. The table
// makes the bit explicit and reversible:
//
//   - a row exists   ⇒ the conv has been an agent.
//   - retired_at=”  ⇒ active agent (shows on the roster).
//   - retired_at set ⇒ retired — demoted to a plain conversation; the
//     .jsonl is untouched, and it can be reinstated.
//
// The backfill is load-bearing: every conv-id that appears in any
// agentic table is enrolled here, so no agent disappears when tclaude
// upgrades. Online-but-otherwise-unrecorded sessions can't be
// tmux-probed from a SQL migration — the daemon enrolls those on
// startup instead (see agentd reconcileOnlineEnrollment).
func migrateV29toV30(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_enrollment (
			conv_id       TEXT PRIMARY KEY,
			enrolled_at   TEXT NOT NULL,
			enrolled_via  TEXT NOT NULL DEFAULT '',
			retired_at    TEXT NOT NULL DEFAULT '',
			retired_by    TEXT NOT NULL DEFAULT '',
			retire_reason TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_enrollment_active
			ON agent_enrollment(conv_id) WHERE retired_at = '';
	`); err != nil {
		return fmt.Errorf("migrate v29→v30 (create): %w", err)
	}

	if err := backfillAgentEnrollment(db); err != nil {
		return fmt.Errorf("migrate v29→v30 (backfill): %w", err)
	}

	if _, err := db.Exec(`UPDATE schema_version SET version = 30;`); err != nil {
		return fmt.Errorf("migrate v29→v30 (version): %w", err)
	}
	return nil
}

// backfillAgentEnrollment enrolls every conv-id that appears in any
// agentic table — group memberships, ownerships, permission and sudo
// grants, head aliases, succession + clone history, cron jobs, the
// workdir tracker and the message log. This is what keeps an agent
// from disappearing when tclaude upgrades to the enrollment model.
//
// Superseded conversations are deliberately excluded: a conv that
// appears as old_conv_id in agent_conv_succession has been reincarnated
// — its identity moved to the chain head, and the live reincarnate
// path deletes its enrollment row. Enrolling it here would resurrect a
// ghost agent that clutters the roster and can't be retired (every
// enrollment verb redirects forward through the succession chain to
// the head). The exclusion is a WHERE clause, not just an omitted
// UNION arm, so a predecessor still loses its enrollment even when it
// is also referenced by some other agentic table (it almost always is
// — agent_messages, agent_workdir, …). The chain head itself only
// appears as new_conv_id, never old_conv_id, so it is kept.
//
// INSERT OR IGNORE so a conv referenced by several tables enrolls
// exactly once; the whole thing is idempotent and safe to re-run. The
// UNION's result column is named by its first SELECT (`conv_id`),
// which the outer SELECT and WHERE then filter on. Split out from the
// migration so it is independently testable.
func backfillAgentEnrollment(db *sql.DB) error {
	now := time.Now().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		INSERT OR IGNORE INTO agent_enrollment (conv_id, enrolled_at, enrolled_via)
		SELECT conv_id, ?, 'migration' FROM (
			      SELECT conv_id        FROM agent_group_members
			UNION SELECT conv_id        FROM agent_group_owners
			UNION SELECT conv_id        FROM agent_permissions
			UNION SELECT conv_id        FROM agent_sudo_grants
			UNION SELECT anchor_conv_id FROM agent_head_aliases
			UNION SELECT new_conv_id    FROM agent_conv_succession
			UNION SELECT source_conv_id FROM agent_clone_history
			UNION SELECT owner_conv     FROM agent_cron_jobs
			UNION SELECT target_conv    FROM agent_cron_jobs
			UNION SELECT conv_id        FROM agent_workdir
			UNION SELECT from_conv      FROM agent_messages
			UNION SELECT to_conv        FROM agent_messages
		)
		WHERE conv_id IS NOT NULL AND conv_id != ''
		  AND conv_id NOT IN (
			SELECT old_conv_id FROM agent_conv_succession
			WHERE old_conv_id IS NOT NULL AND old_conv_id != ''
		  );
	`, now)
	return err
}

// migrateV28toV29 adds agent_groups.default_context — an optional
// block of shared startup guidance the human attaches to a group.
// When set, agents spawned into the group get it delivered to their
// inbox as part of the spawn startup briefing, unless the spawn opts
// out. Empty string = no group context (the pre-feature behaviour).
// Multi-line text is fine: the inbox stores it as plain text so
// embedded newlines survive intact.
func migrateV28toV29(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_groups
			ADD COLUMN default_context TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 29;
	`)
	if err != nil {
		return fmt.Errorf("migrate v28→v29: %w", err)
	}
	return nil
}

// migrateV27toV28 widens agent_workdir from a bare directory into a
// full "current location" record: alongside the most-recent edit dir
// it now stores that dir's git worktree root and branch. The
// PostToolUse hook computes both at edit time, so every read surface
// (dashboard, `agent ls`, `agent dir`) can report where an agent is
// *actually* working — and on which branch — without shelling out to
// git per refresh. This keeps tracking correct when the agent hops
// between sub-repos of a monorepo launch dir, where Claude Code's own
// per-turn gitBranch stamp (the launch dir's branch) goes stale.
//
// Both columns default to ” so rows written by a pre-v28 hook keep
// working — readers fall back to an on-demand git resolution then.
func migrateV27toV28(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_workdir ADD COLUMN worktree_root TEXT NOT NULL DEFAULT '';
		ALTER TABLE agent_workdir ADD COLUMN branch        TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 28;
	`)
	if err != nil {
		return fmt.Errorf("migrate v27→v28: %w", err)
	}
	return nil
}

// migrateV26toV27 adds agent_groups.default_cwd — the working
// directory pre-filled into the spawn form for agents created
// directly into a group. Empty string = no default (spawn falls
// back to the daemon's own cwd, the pre-feature behaviour).
// handleGroupSpawn substitutes this when the spawn request leaves
// cwd blank, so the default reaches the CLI (`tclaude agent spawn`)
// and API too, not just the dashboard's prefill.
func migrateV26toV27(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_groups
			ADD COLUMN default_cwd TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 27;
	`)
	if err != nil {
		return fmt.Errorf("migrate v26→v27: %w", err)
	}
	return nil
}

// migrateV25toV26 adds agent_workdir — the most-recent directory an
// agent has been editing files in, distinct from sessions.cwd (where
// Claude Code was launched). The PostToolUse hook callback upserts the
// dir of every file the agent edits; the daemon's /v1/.../dir endpoints
// read it back so `tclaude agent dir` can report where an agent is
// actually building, not just where it started.
//
// One row per conv_id (PRIMARY KEY); the upsert overwrites in place, so
// the table stays bounded by the number of conversations rather than
// the number of edits. Kept as its own table — not a sessions column —
// because SaveSession's INSERT OR REPLACE would otherwise clobber an
// out-of-band column on every hook tick.
func migrateV25toV26(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_workdir (
			conv_id    TEXT PRIMARY KEY,
			dir        TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

		UPDATE schema_version SET version = 26;
	`)
	if err != nil {
		return fmt.Errorf("migrate v25→v26: %w", err)
	}
	return nil
}

// migrateV24toV25 adds agent_group_links — directed comm edges between
// groups. Lets two flat groups exchange messages without merging
// memberships or installing owner bridges. See
// docs/plans/TODO/med-prio/group-links.md for the design.
//
// Shape:
//   - (from_group_id, to_group_id, mode) is unique — at most one row
//     per direction+mode. Reverse edge is a separate row (callers pass
//     --bidir to create both).
//   - mode is a text discriminator. v1 parses 'members->members' and
//     'owners->members'; future modes get added without a schema bump.
//   - by_conv records the author (empty for human/dashboard, conv-id
//     for an agent-authored grant). Same convention as
//     agent_group_owners.granted_by and agent_head_aliases.by_conv.
//   - ON DELETE CASCADE on both group FKs: deleting a group drops the
//     links that involve it.
func migrateV24toV25(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_group_links (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			from_group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			to_group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			mode            TEXT    NOT NULL,
			created_at      TEXT    NOT NULL,
			by_conv         TEXT    NOT NULL DEFAULT '',
			UNIQUE (from_group_id, to_group_id, mode)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_group_links_from
			ON agent_group_links(from_group_id);
		CREATE INDEX IF NOT EXISTS idx_agent_group_links_to
			ON agent_group_links(to_group_id);

		UPDATE schema_version SET version = 25;
	`)
	if err != nil {
		return fmt.Errorf("migrate v24→v25: %w", err)
	}
	return nil
}

// migrateV23toV24 adds sessions.nudged_pct — the highest
// context_pct threshold the daemon has already fired a
// "consider reincarnating" nudge for. Lets the Stop-hook nudge
// path skip thresholds it's already crossed, so flicker around
// a boundary (e.g. 49.5 → 50.1 → 49.8) doesn't re-ping.
// ResetCompact zeroes this alongside context_pct so a compacted
// session can be re-nudged on its next climb.
func migrateV23toV24(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN nudged_pct REAL NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 24;
	`)
	if err != nil {
		return fmt.Errorf("migrate v23→v24: %w", err)
	}
	return nil
}

// migrateV22toV23 adds agent_sudo_grants — time-bounded permission
// elevations modeled after Unix sudo / GCP PAM. An agent requests a
// bundle of slugs for a bounded duration; the request always pops a
// human-approval popup; on approve we insert one row per slug with
// the same expires_at. requirePermission consults active rows
// (`expires_at > now()` AND revoked_at IS NULL) as a third source
// alongside defaults and per-conv grants.
//
// Shape:
//   - id PRIMARY KEY for clean revocation by id from the CLI / dashboard.
//   - conv_id is the agent the elevation applies to.
//   - slug is the permission being elevated.
//   - granted_at / expires_at are RFC3339Nano stamps; expires_at is
//     the time-bound check at lookup.
//   - granted_by carries audit context, e.g. "human:popup-id=<n>" for
//     popup-approved grants, "human:cli" for direct admin grants.
//   - reason is the caller-supplied justification surfaced in the popup
//     and audit views.
//   - revoked_at is non-NULL when explicitly revoked early (distinct
//     from expired-by-time so audit can tell those apart).
//
// Partial index makes the active-grants probe O(matching rows).
func migrateV22toV23(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_sudo_grants (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			conv_id     TEXT NOT NULL,
			slug        TEXT NOT NULL,
			granted_at  TEXT NOT NULL,
			expires_at  TEXT NOT NULL,
			granted_by  TEXT NOT NULL,
			reason      TEXT NOT NULL DEFAULT '',
			revoked_at  TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_sudo_active
			ON agent_sudo_grants(conv_id, expires_at)
			WHERE revoked_at = '';

		UPDATE schema_version SET version = 23;
	`)
	if err != nil {
		return fmt.Errorf("migrate v22→v23: %w", err)
	}
	return nil
}

// migrateV21toV22 adds agent_head_aliases — a small naming layer
// that maps a stable handle (e.g. "ceo", "po") to a conv-id chain
// anchor. Lookups walk the chain via db.ResolveLatestConv at read
// time, so the handle always resolves to the current head no matter
// how many times the conv has been reincarnated. Complements the
// existing per-group agent_group_members.alias by being GLOBAL —
// not scoped to a group, useful for convs that aren't (yet) in any
// group.
//
// Shape:
//   - handle is the user-visible name; PRIMARY KEY enforces uniqueness
//     across the daemon. Lower-cased on insert so case folding doesn't
//     surprise the human.
//   - anchor_conv_id is the conv we point at; ResolveLatestConv walks
//     forward from there, so we don't need to update on every
//     reincarnate (the succession row added by reincarnate is enough).
//   - by_conv records who set the handle (empty string for human via
//     dashboard / CLI; conv-id when an agent set it). Same shape as
//     agent_group_owners.granted_by.
func migrateV21toV22(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_head_aliases (
			handle         TEXT PRIMARY KEY,
			anchor_conv_id TEXT NOT NULL,
			created_at     TEXT NOT NULL,
			by_conv        TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_head_aliases_anchor
			ON agent_head_aliases(anchor_conv_id);

		UPDATE schema_version SET version = 22;
	`)
	if err != nil {
		return fmt.Errorf("migrate v21→v22: %w", err)
	}
	return nil
}

// migrateV20toV21 adds agent_messages.original_to_conv — when the send
// path's `db.ResolveLatestConv` rewrites the addressed conv-id onto a
// live successor, the recipient still wants to see who the message
// was originally for. Empty for sends that didn't get redirected.
//
// Shape: TEXT NOT NULL DEFAULT ” (empty == "this row was sent
// directly, no redirection happened"). Cheap to filter on, no index
// needed — reads are by primary key.
func migrateV20toV21(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_messages
			ADD COLUMN original_to_conv TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 21;
	`)
	if err != nil {
		return fmt.Errorf("migrate v20→v21: %w", err)
	}
	return nil
}

// migrateV19toV20 adds agent_group_audit — append-only history of group
// rename events. Lets `tclaude agent groups rename` keep the rename
// debuggable without needing to scrape slog ("what was this group
// called before?"). Same shape as agent_conv_succession.
//
// Shape:
//   - group_id is the stable FK; survives the rename so a later lookup
//     can chain backward through the audit rows for the full history.
//   - by_conv is the conv-id that authored the rename (empty for the
//     human path — humans bypass permission checks).
func migrateV19toV20(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_group_audit (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			old_name   TEXT NOT NULL,
			new_name   TEXT NOT NULL,
			by_conv    TEXT NOT NULL DEFAULT '',
			at         TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_group_audit_group
			ON agent_group_audit(group_id, at);

		UPDATE schema_version SET version = 20;
	`)
	if err != nil {
		return fmt.Errorf("migrate v19→v20: %w", err)
	}
	return nil
}

// migrateV18toV19 adds agent_clone_history — append-only audit of every
// clone attempt that passed the rate-limit check. The daemon's clone
// handler does an atomic INSERT-WHERE-NOT-EXISTS against this table to
// gate "1 clone per cooldown" at the source-conv level (see
// ClaimCloneSlot), preventing a runaway clone loop from forking the
// same conv unboundedly.
//
// Shape:
//   - source_conv_id is the target being cloned. Index keys lookups by
//     (source, cloned_at) for the recency probe.
//   - cloned_at is the wall-clock instant the clone was claimed
//     (RFC3339Nano so closely-spaced attempts compare correctly).
//
// Append-only by construction: rows are never updated or deleted in
// the production code path. A future cleanup verb could prune rows
// older than N days if the table grows uncomfortably; not pulled by
// need yet.
func migrateV18toV19(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_clone_history (
			source_conv_id TEXT NOT NULL,
			cloned_at      TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_clone_history_source
			ON agent_clone_history(source_conv_id, cloned_at);

		UPDATE schema_version SET version = 19;
	`)
	if err != nil {
		return fmt.Errorf("migrate v18→v19: %w", err)
	}
	return nil
}

// migrateV17toV18 adds agent_messages.{to_recipients,cc_recipients}
// — the email-style multi-recipient model. Each row in agent_messages
// stays the per-recipient view (one row per (message × recipient) so
// delivered_at / read_at stay per-recipient), but the AUDIENCE of
// the original send is now denormalized onto every recipient's row:
//
//   - to_recipients: JSON array of conv-ids that were primary
//     recipients of the original send. The recipient on this row knows
//     they were a primary if their own conv-id appears here.
//   - cc_recipients: JSON array of conv-ids that were CC'd.
//
// Both empty for legacy single-recipient messages — the existing
// to_conv column is canonical for delivery + filtering, the
// recipients arrays are display-only ("From X, To: Y, CC: Z, W"
// rendering in inbox read).
//
// Encoded as JSON arrays inside TEXT for forward-compat (we may
// switch to a normalized recipients table later, but the JSON form
// keeps the v1 migration cheap and the read path simple).
func migrateV17toV18(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_messages
			ADD COLUMN to_recipients TEXT NOT NULL DEFAULT '';
		ALTER TABLE agent_messages
			ADD COLUMN cc_recipients TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 18;
	`)
	if err != nil {
		return fmt.Errorf("migrate v17→v18: %w", err)
	}
	return nil
}

// migrateV16toV17 adds conv_index.archived_at — a soft-delete /
// expired flag for individual conversations. Mirrors
// agent_groups.archived_at (schema v16) so the same "archived"
// semantics apply to both groups and convs.
//
// Reincarnate writes this column on the OLD conv after spawning the
// successor (alongside the cosmetic /rename injection that adds the
// `-x` title suffix). Manual archive (`tclaude conv archive
// <selector>`, future verb) will set this column directly without
// renaming the title.
//
// Empty string = active. Non-empty (RFC3339 timestamp) = archived.
// Indexed so the eventual `WHERE archived_at = ”` filter on
// listing endpoints stays cheap as the table grows.
//
// Crucially: UpsertConvIndex does NOT include archived_at in its ON
// CONFLICT update, so a routine .jsonl rescan never clobbers the
// archived state. The column changes only via the dedicated
// SetConvIndexArchived setter.
func migrateV16toV17(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE conv_index
			ADD COLUMN archived_at TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS idx_conv_index_archived
			ON conv_index(archived_at);

		UPDATE schema_version SET version = 17;
	`)
	if err != nil {
		return fmt.Errorf("migrate v16→v17: %w", err)
	}
	return nil
}

// migrateV15toV16 adds agent_groups.archived_at — a soft-delete /
// freeze flag. Distinct from `groups rm` (which destroys membership +
// history) and from `groups stop` (which only ends the running tmux
// sessions, leaving membership intact and writable). Archive freezes
// membership + ownership AND hides the group from default listings,
// while preserving the message history for forensic queries.
//
// Empty string = active. Non-empty (RFC3339 timestamp) = archived.
// Plain TEXT column rather than separate epoch + flag because a
// human reading the row directly should see when, not just whether.
//
// Indexed so the eventual `WHERE archived_at = ”` filter on
// listing endpoints stays cheap as the table grows.
func migrateV15toV16(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_groups
			ADD COLUMN archived_at TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS idx_agent_groups_archived
			ON agent_groups(archived_at);

		UPDATE schema_version SET version = 16;
	`)
	if err != nil {
		return fmt.Errorf("migrate v15→v16: %w", err)
	}
	return nil
}

// migrateV14toV15 adds agent_conv_succession — a forward-pointing
// chain old_conv_id → new_conv_id captured every time we replace one
// conv with a fresh one (today: reincarnate; future: clone-replace).
//
// Why: reincarnate eagerly migrates groups / permissions / ownership
// from the old conv to the new one, but other surfaces hold stable
// conv-id references that we can't always rewrite at migration time
// — cron jobs, historical inbox rows, the user typing an old conv-id
// at the CLI. The succession table lets a resolver walk forward from
// any historical id to the current live conv.
//
// Shape:
//   - old_conv_id is the PRIMARY KEY: a conv can only succeed once.
//     If a chain forms (A→B→C), each row stores its direct successor;
//     ResolveLatestConv walks the chain.
//   - new_conv_id is indexed so reverse lookups ("what predecessors
//     does this conv have?") are cheap.
//   - reason is a short tag — `reincarnate`, `clone-replace` (future).
//   - succeeded_at is the wall-clock time the migration ran.
//
// We do NOT cascade-delete: keeping the row even after the new conv
// is gone is what makes "tclaude conv resume <old-id>" still
// resolvable from a forensic/log-spelunking angle.
func migrateV14toV15(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_conv_succession (
			old_conv_id   TEXT PRIMARY KEY,
			new_conv_id   TEXT NOT NULL,
			reason        TEXT NOT NULL DEFAULT '',
			succeeded_at  TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_conv_succession_new
			ON agent_conv_succession(new_conv_id);

		UPDATE schema_version SET version = 15;
	`)
	if err != nil {
		return fmt.Errorf("migrate v14→v15: %w", err)
	}
	return nil
}

// migrateV13toV14 adds agent_cron_runs — execution history for the
// cron scheduler. Each successful (or failed) fire adds a row, so
// the dashboard / `cron logs` verb can show "last few runs" without
// having to mine slog output. agent_cron_jobs.last_run_at /
// last_run_status stay as denorm caches for the listing view (avoids
// a sub-select on every list).
//
// fired_at is when the scheduler picked up the job; status mirrors
// the LastRunStatus tags (ok / send_failed / no_target). error_msg
// is the raw error string when status != ok, empty otherwise.
//
// FK on job_id with ON DELETE CASCADE so deleting a job purges its
// history — no need for a separate cleanup pass.
func migrateV13toV14(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_cron_runs (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id    INTEGER NOT NULL REFERENCES agent_cron_jobs(id) ON DELETE CASCADE,
			fired_at  TEXT NOT NULL,
			status    TEXT NOT NULL DEFAULT '',
			error_msg TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_cron_runs_job
			ON agent_cron_runs(job_id, fired_at DESC);

		UPDATE schema_version SET version = 14;
	`)
	if err != nil {
		return fmt.Errorf("migrate v13→v14: %w", err)
	}
	return nil
}

// migrateV12toV13 adds agent_cron_jobs — recurring scheduled jobs
// the agentd scheduler fires on a wall-clock interval. v1 supports
// interval-only schedules (e.g. every 10 minutes); cron expressions
// can be added later as a separate column without migration churn.
//
// owner_conv records who created the job (for audit + display in
// the dashboard); target_conv is who the message lands on. group_id
// is the routing path for delivery — when set, the job inserts an
// agent_messages row so the existing flush nudge pipeline picks it
// up; when 0, the scheduler falls back to direct send-keys.
//
// last_run_at unset (zero time) → "never run, due immediately".
// On every successful fire we set last_run_at = now (NOT now -
// missed-intervals); skipping catch-ups means a daemon restart
// after a long offline period doesn't replay 50 messages at once.
func migrateV12toV13(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_cron_jobs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL DEFAULT '',
			owner_conv       TEXT NOT NULL,
			target_conv      TEXT NOT NULL,
			group_id         INTEGER NOT NULL DEFAULT 0,
			interval_seconds INTEGER NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			enabled          INTEGER NOT NULL DEFAULT 1,
			created_at       TEXT NOT NULL,
			last_run_at      TEXT NOT NULL DEFAULT '',
			last_run_status  TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_cron_jobs_owner ON agent_cron_jobs(owner_conv);
		CREATE INDEX IF NOT EXISTS idx_agent_cron_jobs_target ON agent_cron_jobs(target_conv);

		UPDATE schema_version SET version = 13;
	`)
	if err != nil {
		return fmt.Errorf("migrate v12→v13: %w", err)
	}
	return nil
}

func schemaVersion(db *sql.DB) int {
	var ver int
	err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&ver)
	if err != nil {
		return 0
	}
	return ver
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (1);

		CREATE TABLE IF NOT EXISTS sessions (
			id              TEXT PRIMARY KEY,
			tmux_session    TEXT NOT NULL DEFAULT '',
			pid             INTEGER NOT NULL DEFAULT 0,
			cwd             TEXT NOT NULL DEFAULT '',
			conv_id         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'idle',
			status_detail   TEXT NOT NULL DEFAULT '',
			auto_registered INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_conv_id ON sessions(conv_id);
		CREATE INDEX IF NOT EXISTS idx_sessions_status_updated ON sessions(status, updated_at);

		CREATE TABLE IF NOT EXISTS notify_state (
			session_id  TEXT PRIMARY KEY,
			notified_at TEXT NOT NULL
		);
	`)
	return err
}

// legacySessionJSON matches the JSON structure of the old file-based session state.
type legacySessionJSON struct {
	ID           string    `json:"id"`
	TmuxSession  string    `json:"tmuxSession"`
	PID          int       `json:"pid"`
	Cwd          string    `json:"cwd"`
	ConvID       string    `json:"convId,omitempty"`
	Status       string    `json:"status"`
	StatusDetail string    `json:"statusDetail,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

func migrateV1toV2(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS usage_cache (
			id              INTEGER PRIMARY KEY,
			data            TEXT NOT NULL DEFAULT '{}',
			fetched_at      TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS git_cache (
			repo_hash  TEXT PRIMARY KEY,
			data       TEXT NOT NULL DEFAULT '{}',
			fetched_at TEXT NOT NULL DEFAULT ''
		);

		UPDATE schema_version SET version = 2;
	`)
	if err != nil {
		return fmt.Errorf("migrate v1→v2: %w", err)
	}
	return nil
}

func migrateV2toV3(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conv_index (
			conv_id       TEXT PRIMARY KEY,
			project_dir   TEXT NOT NULL,
			full_path     TEXT NOT NULL,
			file_mtime    INTEGER NOT NULL DEFAULT 0,
			file_size     INTEGER NOT NULL DEFAULT 0,
			first_prompt  TEXT NOT NULL DEFAULT '',
			summary       TEXT NOT NULL DEFAULT '',
			custom_title  TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			created       TEXT NOT NULL DEFAULT '',
			modified      TEXT NOT NULL DEFAULT '',
			git_branch    TEXT NOT NULL DEFAULT '',
			project_path  TEXT NOT NULL DEFAULT '',
			is_sidechain  INTEGER NOT NULL DEFAULT 0,
			indexed_at    TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_conv_index_project_dir ON conv_index(project_dir);

		UPDATE schema_version SET version = 3;
	`)
	if err != nil {
		return fmt.Errorf("migrate v2→v3: %w", err)
	}
	return nil
}

func migrateV3toV4(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN context_pct REAL NOT NULL DEFAULT 0;
		ALTER TABLE sessions ADD COLUMN compact_pending REAL NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 4;
	`)
	if err != nil {
		return fmt.Errorf("migrate v3→v4: %w", err)
	}
	return nil
}

func migrateV4toV5(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conv_embeddings (
			conv_id     TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			chunk_type  TEXT NOT NULL DEFAULT 'content',
			chunk_text  TEXT NOT NULL DEFAULT '',
			embedding   BLOB NOT NULL,
			model       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, chunk_index)
		);
		CREATE INDEX IF NOT EXISTS idx_conv_embeddings_conv_id ON conv_embeddings(conv_id);

		UPDATE schema_version SET version = 5;
	`)
	if err != nil {
		return fmt.Errorf("migrate v4→v5: %w", err)
	}
	return nil
}

func migrateV5toV6(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN subagent_count INTEGER NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 6;
	`)
	if err != nil {
		return fmt.Errorf("migrate v5→v6: %w", err)
	}
	return nil
}

func migrateV6toV7(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN last_hook TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 7;
	`)
	if err != nil {
		return fmt.Errorf("migrate v6→v7: %w", err)
	}
	return nil
}

// migrateV8toV9 adds the agent_permissions table — per-conv permission
// overrides. Previously these lived in ~/.tclaude/config.json under
// agent.permission_overrides; that proved awkward (config rewrites need
// careful merging, and the human edits this file by hand for log_level
// etc.). Storing per-agent grants in SQLite — alongside agent_groups
// and agent_group_members — keeps the data model uniform and lets the
// daemon mutate without touching JSON. config.json keeps only
// DefaultPermissions; legacy permission_overrides values are imported
// here on first open.
// migrateV11toV12 adds absolute token-count columns to sessions.
// Claude Code's statusline JSON (v2.1.132+) exposes total_input_tokens,
// total_output_tokens, and context_window_size; before that the same
// fields existed but were cumulative session totals rather than the
// last-API-response snapshot. Either way we just record whatever the
// hook wrote on its most recent tick — no historical aggregation here.
//
// All three default to 0; rows that haven't been touched by the new
// statusbar code yet just read back zero, which the consumer (the
// agent context-info CLI / handler) treats as "unknown" and falls
// back to the percentage-only display.
func migrateV11toV12(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN tokens_input INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sessions ADD COLUMN tokens_output INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sessions ADD COLUMN context_window_size INTEGER NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 12;
	`)
	if err != nil {
		return fmt.Errorf("migrate v11→v12: %w", err)
	}
	return nil
}

// migrateV10toV11 adds agent_group_owners — a per-group "owner" set
// distinct from agent_group_members. An owner can send messages to
// the group's members (and to the group via multicast) without being
// a member themselves. Useful for coordinator agents that orchestrate
// teams without needing to be addressed as a peer.
//
// Distinct table (rather than a column on agent_group_members) so
// "X is an owner but not a member" is representable. When an agent
// is both, the UI shows them in the members list tagged "owner".
func migrateV10toV11(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_group_owners (
			group_id    INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			conv_id     TEXT NOT NULL,
			granted_at  TEXT NOT NULL,
			granted_by  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (group_id, conv_id)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_group_owners_conv ON agent_group_owners(conv_id);

		UPDATE schema_version SET version = 11;
	`)
	if err != nil {
		return fmt.Errorf("migrate v10→v11: %w", err)
	}
	return nil
}

// migrateV9toV10 adds agent_messages.parent_id for thread chaining.
// Nullable-equivalent (default 0) since we never want a foreign-key
// constraint here — pruning a parent shouldn't cascade-delete its
// reply chain. parent_id = 0 means "top of thread."
func migrateV9toV10(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_messages ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0;
		CREATE INDEX IF NOT EXISTS idx_agent_messages_parent ON agent_messages(parent_id);

		UPDATE schema_version SET version = 10;
	`)
	if err != nil {
		return fmt.Errorf("migrate v9→v10: %w", err)
	}
	return nil
}

func migrateV8toV9(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_permissions (
			conv_id    TEXT NOT NULL,
			slug       TEXT NOT NULL,
			granted_at TEXT NOT NULL,
			granted_by TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, slug)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_permissions_slug ON agent_permissions(slug);

		UPDATE schema_version SET version = 9;
	`)
	if err != nil {
		return fmt.Errorf("migrate v8→v9: %w", err)
	}
	return nil
}

func migrateV7toV8(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_groups (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			descr       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS agent_group_members (
			group_id    INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			conv_id     TEXT NOT NULL,
			alias       TEXT NOT NULL DEFAULT '',
			role        TEXT NOT NULL DEFAULT '',
			descr       TEXT NOT NULL DEFAULT '',
			joined_at   TEXT NOT NULL,
			PRIMARY KEY (group_id, conv_id)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_group_members_conv ON agent_group_members(conv_id);

		CREATE TABLE IF NOT EXISTS agent_messages (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT,
			from_conv    TEXT NOT NULL,
			to_conv      TEXT NOT NULL,
			subject      TEXT NOT NULL DEFAULT '',
			body         TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL,
			delivered_at TEXT NOT NULL DEFAULT '',
			read_at      TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_messages_to_conv ON agent_messages(to_conv, created_at);

		UPDATE schema_version SET version = 8;
	`)
	if err != nil {
		return fmt.Errorf("migrate v7→v8: %w", err)
	}
	return nil
}

func importLegacyData(db *sql.DB) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil // no home dir, nothing to import
	}

	importedSessions := importLegacySessions(db, home)
	importedNotify := importLegacyNotifyState(db, home)

	// Move debug.log from old location (~/.tclaude/claude-sessions/debug.log)
	// to new location (~/.tclaude/debug.log) before renaming the directory.
	oldDebugLog := filepath.Join(home, ".tclaude", "claude-sessions", "debug.log")
	newDebugLog := filepath.Join(home, ".tclaude", "debug.log")
	if _, err := os.Stat(oldDebugLog); err == nil {
		if _, err := os.Stat(newDebugLog); os.IsNotExist(err) {
			if err := os.Rename(oldDebugLog, newDebugLog); err != nil {
				slog.Warn("failed to move debug.log", "error", err)
			}
		}
	}

	if importedSessions {
		oldDir := filepath.Join(home, ".tclaude", "claude-sessions")
		newDir := oldDir + ".migrated"
		if err := os.Rename(oldDir, newDir); err != nil {
			slog.Warn("failed to rename legacy sessions dir", "error", err)
		}
	}
	if importedNotify {
		oldDir := filepath.Join(home, ".tclaude", "notify-state")
		newDir := oldDir + ".migrated"
		if err := os.Rename(oldDir, newDir); err != nil {
			slog.Warn("failed to rename legacy notify-state dir", "error", err)
		}
	}

	return nil
}

func importLegacySessions(db *sql.DB, home string) bool {
	dir := filepath.Join(home, ".tclaude", "claude-sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	// Collect .auto markers first
	autoMarkers := make(map[string]bool)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".auto") {
			id := strings.TrimSuffix(entry.Name(), ".auto")
			autoMarkers[id] = true
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()

	imported := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var s legacySessionJSON
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), ".json")
		autoReg := 0
		if autoMarkers[id] {
			autoReg = 1
		}

		_, err = tx.Exec(`INSERT OR IGNORE INTO sessions
			(id, tmux_session, pid, cwd, conv_id, status, status_detail, auto_registered, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.TmuxSession, s.PID, s.Cwd, s.ConvID,
			s.Status, s.StatusDetail, autoReg,
			s.Created.Format(time.RFC3339Nano), s.Updated.Format(time.RFC3339Nano))
		if err != nil {
			slog.Warn("failed to import session", "id", s.ID, "error", err)
			continue
		}
		imported++
	}

	if imported == 0 {
		return false
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("failed to commit session import", "error", err)
		return false
	}

	slog.Info(fmt.Sprintf("imported %d legacy sessions into SQLite", imported))
	return true
}

func importLegacyNotifyState(db *sql.DB, home string) bool {
	dir := filepath.Join(home, ".tclaude", "notify-state")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	tx, err := db.Begin()
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()

	imported := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		_, err = tx.Exec(`INSERT OR IGNORE INTO notify_state (session_id, notified_at) VALUES (?, ?)`,
			entry.Name(), info.ModTime().Format(time.RFC3339Nano))
		if err != nil {
			continue
		}
		imported++
	}

	if imported == 0 {
		return false
	}

	if err := tx.Commit(); err != nil {
		return false
	}

	slog.Info(fmt.Sprintf("imported %d legacy notify states into SQLite", imported))
	return true
}
