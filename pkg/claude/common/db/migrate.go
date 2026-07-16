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

const currentVersion = 125

// DefaultHarness is the value of the `harness` column for a row that
// predates multi-harness support or was produced by the Claude Code scan
// path. It matches the `harness TEXT NOT NULL DEFAULT 'claude'` column
// default (migrateV56toV57) and harness.DefaultName in pkg/claude/harness
// — kept as a literal here because the db/convops layers cannot import
// harness without an import cycle (harness → common → convops).
const DefaultHarness = "claude"

func migrate(db *sql.DB) error {
	r := migrationReporter
	ver := schemaVersion(db)
	if ver == currentVersion {
		// Already at head — the overwhelmingly common restart. No migration
		// runs, but announce the no-op so an operator watching agentd start
		// can tell "nothing to migrate" apart from "migrated" or "failed
		// before reporting". The reporter is nil for every CLI command, so
		// this stays agentd-startup-only (see MigrationReporter).
		r.reportAlreadyCurrent(ver, currentVersion)
		return nil
	}
	// The DB's true starting version, reported to the caller before any work
	// (0 for a brand-new DB, before createSchema bumps it to 1).
	from := ver

	if ver == 0 {
		if err := createSchema(db); err != nil {
			return err
		}
		if err := importLegacyData(db); err != nil {
			return err
		}
		ver = 1 // createSchema sets version to 1
	}

	// Only walk the chain announcing progress when there is actually forward
	// work to do. A DB pathologically PAST head — its schema written by a newer
	// binary — applies nothing either; report it as a no-op too (with the DB's
	// actual version) rather than returning wordlessly.
	if ver >= currentVersion {
		r.reportAlreadyCurrent(ver, currentVersion)
		return nil
	}
	r.reportBegin(from, currentVersion)
	for _, step := range migrationSteps {
		if ver >= step.version {
			continue
		}
		r.reportApplying(step.version)
		if err := step.apply(db); err != nil {
			r.reportFailed(step.version, err)
			return err
		}
		r.reportApplied(step.version)
		ver = step.version
	}
	r.reportDone(currentVersion)
	return nil
}

// migrateV86toV87 adds sessions.subagents_json — the per-session ledger of
// currently-running sub-agents ({agent_id: {type, seen}}, see SubagentSet in
// subagents.go). It replaces trusting the bare subagent_count +1/-1 stream:
// Claude Code fires no hooks on a user interrupt and SubagentStop has no
// termination guarantee, so a lost event used to drift the count (and the
// dashboard's "🤖+N" badge) permanently. The ledger self-heals — Sight()
// re-adds a sub-agent whose Start was lost, the TTL ages out one whose Stop
// was lost — and subagent_count becomes a derived cache of it.
//
// TEXT NOT NULL DEFAULT ” — a pre-existing row reads as "no ledger yet",
// which the read side falls back from (see stateForConvIn). One transaction;
// the ADD COLUMN is guarded by a sqlite_master table-existence probe AND a
// pragma_table_info column probe (the migrateV65toV66 / migrateV56toV57
// conventions), so a half-applied run converges on re-run instead of wedging
// on "duplicate column".
func migrateV86toV87(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v86→v87 (add subagents_json): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v86→v87 (add subagents_json): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'subagents_json'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v86→v87 (add subagents_json): probe column: %w", err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN subagents_json TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v86→v87 (add subagents_json): add column: %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 87`); err != nil {
		return fmt.Errorf("migrate v86→v87 (add subagents_json): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v86→v87 (add subagents_json): commit: %w", err)
	}
	return nil
}

// migrateV71toV72 introduces the stable agent-identity layer (JOH-26):
// `agents` (the durable actor) + `agent_conversations` (every conversation
// generation mapped to its actor). It decouples identity from the harness
// conv-id, which today rotates on reincarnate and Claude Code's /clear and
// forces db.MigrateAgentIdentity to physically rekey identity rows.
//
// This migration is ADDITIVE and behaviour-preserving: it stands up the
// tables and backfills them from the current conv-keyed state. Authorization
// still reads the conv-keyed identity tables unchanged — the cutover to
// agent_id-keyed authz is a later, separate migration.
//
// Not wrapped in one transaction (mirrors migrateV29toV30): the CREATE TABLE
// IF NOT EXISTS statements and backfillAgents are each idempotent, so a
// crash mid-backfill leaves schema_version at 71 and the whole pass re-runs
// and converges. agents.current_conv_id carries a UNIQUE constraint (each
// conversation generation heads at most one actor); the backfill's orphan
// guard keeps a re-run from colliding on it.
func migrateV71toV72(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agents (
			agent_id        TEXT PRIMARY KEY,
			current_conv_id TEXT NOT NULL UNIQUE,
			created_at      TEXT NOT NULL,
			created_via     TEXT NOT NULL DEFAULT '',
			retired_at      TEXT NOT NULL DEFAULT '',
			retired_by      TEXT NOT NULL DEFAULT '',
			retire_reason   TEXT NOT NULL DEFAULT '',
			pending_name    TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS agent_conversations (
			conv_id   TEXT PRIMARY KEY,
			agent_id  TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			role      TEXT NOT NULL DEFAULT '',
			reason    TEXT NOT NULL DEFAULT '',
			linked_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_conversations_agent
			ON agent_conversations(agent_id);
	`); err != nil {
		return fmt.Errorf("migrate v71→v72 (create): %w", err)
	}

	if err := backfillAgents(db); err != nil {
		return fmt.Errorf("migrate v71→v72 (backfill): %w", err)
	}

	if _, err := db.Exec(`UPDATE schema_version SET version = 72`); err != nil {
		return fmt.Errorf("migrate v71→v72 (version): %w", err)
	}
	return nil
}

// migrateV70toV71 adds session_cost_daily.model — the LLM model display
// name ("Opus 4.8 (1M context)", "Sonnet 4.6", …) denormalised onto the
// cost-history row at write time, the model sibling of the conv_id that
// v50→v51 already denormalises. Without it the Costs tab's per-agent
// MODEL column resolved the model with a LIVE lookup against the sessions
// row (SessionModels), so the instant a session row was deleted — agent
// retired/killed, or a conv resumed/reincarnated under a fresh session id
// — its surviving cost history could no longer name a model and the cell
// rendered blank. Snapshotting the model into the history row (like the
// cost itself) makes it survive the sessions row's deletion, so a retired
// agent keeps showing what it ran on. Default "" so every existing row
// and every reader that doesn't yet select the column keeps working.
//
// The backfill seeds the column from every live sessions row that still
// carries a model (the same lookup SessionModels did), so today's history
// names its model immediately instead of waiting for each session's next
// statusline tick; rows whose session was already deleted keep "" (their
// model is gone for good — only NEW spend going forward can be captured).
//
// Single transaction, pragma_table_info-guarded (the migrateV56toV57
// convention): SQLite has no ADD COLUMN IF NOT EXISTS, so a half-applied /
// re-run must converge instead of wedging on "duplicate column name". The
// ALTER is further guarded on the table existing at all: a real DB always
// has session_cost_daily by here (v50→v51 created it), but the migration
// chain's partial-schema heal tests seed only the tables a downstream
// migration ALTERs, so the column add (and backfill) no-ops when the
// table is absent and just advances the version.
func migrateV70toV71(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_cost_daily'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): probe table: %w", err)
	}
	var haveCol, haveSessions int
	if haveTable > 0 {
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('session_cost_daily') WHERE name = 'model'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): probe column: %w", err)
		}
		// The backfill reads from sessions; in a real DB it always exists by
		// here, but a partial-schema heal DB could have session_cost_daily
		// without it, so probe and skip the backfill rather than wedge on
		// "no such table: sessions".
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
		).Scan(&haveSessions); err != nil {
			return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): probe sessions table: %w", err)
		}
	}
	if haveTable > 0 && haveCol == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE session_cost_daily ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): add column: %w", err)
		}
		// Backfill from the live sessions row where one still carries a
		// model — the same source the per-agent breakdown read live until
		// now. History whose session was already deleted keeps "".
		if haveSessions > 0 {
			if _, err := tx.Exec(`
				UPDATE session_cost_daily SET model = (
					SELECT s.model FROM sessions s WHERE s.id = session_cost_daily.session_id
				)
				WHERE session_id IN (SELECT id FROM sessions WHERE model <> '')`,
			); err != nil {
				return fmt.Errorf("migrate v70→v71 (backfill session_cost_daily.model): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 71`); err != nil {
		return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v70→v71 (add session_cost_daily.model): commit: %w", err)
	}
	return nil
}

// migrateV69toV70 adds audit_log — the persistent trail of daemon-proxied
// tclaude commands (JOH-268): who ran what against which target, recorded
// from agentd's request middleware. Mirrors the agent_transfer_log shape
// (migrateV39toV40): a denormalized append-only log with an index on `at`
// so the periodic retention prune (DELETE WHERE at < cutoff) stays cheap.
// Actor/target labels are snapshots so a row stays readable after the
// agent it names is renamed/retired/deleted. Rows are read newest-first
// by id, never by `at` (RFC3339Nano TEXT lexical order misorders rows
// inside the same whole second — a known hazard in this DB).
func migrateV69toV70(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_log (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			at           TEXT NOT NULL,
			actor_kind   TEXT NOT NULL DEFAULT '',
			actor_conv   TEXT NOT NULL DEFAULT '',
			actor_label  TEXT NOT NULL DEFAULT '',
			verb         TEXT NOT NULL DEFAULT '',
			target_conv  TEXT NOT NULL DEFAULT '',
			target_label TEXT NOT NULL DEFAULT '',
			group_name   TEXT NOT NULL DEFAULT '',
			detail       TEXT NOT NULL DEFAULT '',
			method       TEXT NOT NULL DEFAULT '',
			path         TEXT NOT NULL DEFAULT '',
			status       INTEGER NOT NULL DEFAULT 0,
			source       TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_audit_log_at
			ON audit_log(at);

		UPDATE schema_version SET version = 70;
	`); err != nil {
		return fmt.Errorf("migrate v69→v70 (add audit_log): %w", err)
	}
	return nil
}

// migrateV68toV69 adds the WHAT-IF cost column to the two cost-bearing tables:
//
//   - sessions.virtual_cost_usd — the cumulative pay-per-token-EQUIVALENT cost
//     of a subscription session, the sibling of sessions.cost_usd (the real
//     pay-per-token spend). Claude Code emits cost.total_cost_usd on every
//     statusline render regardless of billing mode; on a subscription the
//     statusbar discarded it before (the display gate is the subscription's
//     rate-limit buckets), so this column captures it instead — "what this
//     session WOULD have cost on pay-per-token". A session normally writes
//     just one of the two columns (billing mode is stable per account); only
//     a mid-session billing-state flip could touch both, which the two
//     independent delta walks tolerate.
//   - session_cost_daily.virtual_cost_usd — the same value snapshotted onto the
//     per-(session, day) row, so the Costs tab's WHAT-IF view recovers per-day
//     deltas exactly as the real-cost view does over cost_usd.
//
// Both are REAL NOT NULL DEFAULT 0 (mirroring cost_usd, added in v49→v50 /
// v50→v51), so a pre-existing row reads as zero virtual cost — correct, since
// nothing captured it yet. One transaction; each ADD COLUMN is guarded by a
// sqlite_master table-existence probe (the migrateV61toV62 convention) AND a
// pragma_table_info column probe (the migrateV56toV57 convention), so a
// half-applied run converges on re-run instead of wedging on "duplicate column".
func migrateV68toV69(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v68→v69 (add virtual_cost_usd): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// table-name literals (never user input) so the pragma probe and ALTER share
	// the string-literal convention every prior column-add migration uses — a
	// bound `?` inside the table-valued pragma_table_info is version-fragile.
	for _, probe := range []struct{ table, existsSQL, countSQL, alterSQL string }{
		{
			"sessions",
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'virtual_cost_usd'`,
			`ALTER TABLE sessions ADD COLUMN virtual_cost_usd REAL NOT NULL DEFAULT 0`,
		},
		{
			"session_cost_daily",
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_cost_daily'`,
			`SELECT COUNT(*) FROM pragma_table_info('session_cost_daily') WHERE name = 'virtual_cost_usd'`,
			`ALTER TABLE session_cost_daily ADD COLUMN virtual_cost_usd REAL NOT NULL DEFAULT 0`,
		},
	} {
		var haveTable int
		if err := tx.QueryRow(probe.existsSQL).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v68→v69 (probe %s): %w", probe.table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveCol int
		if err := tx.QueryRow(probe.countSQL).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v68→v69 (add %s.virtual_cost_usd): probe column: %w", probe.table, err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(probe.alterSQL); err != nil {
				return fmt.Errorf("migrate v68→v69 (add %s.virtual_cost_usd): add column: %w", probe.table, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 69`); err != nil {
		return fmt.Errorf("migrate v68→v69 (add virtual_cost_usd): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v68→v69 (add virtual_cost_usd): commit: %w", err)
	}
	return nil
}

// migrateV67toV68 adds export_jobs.worker_conv_id — the conv-id of the isolated
// CLONE the daemon spawns to produce a per-agent export on, so the live original
// is never disturbed (JOH-266, the clone-based-export follow-up to JOH-265).
//
// conv_id stays the ORIGINAL (the export's history list + download attach to it);
// worker_conv_id is the throwaway clone that is nudged, submits the artifact, and
// is auto-deleted once the job is ready. The new 'cloning' status (a string in
// the existing status column — no schema change) is the leading lifecycle phase
// while the clone is being spawned.
//
// One transaction; the ADD COLUMN is guarded by BOTH a sqlite_master
// table-existence probe AND a pragma_table_info column probe (the migrateV65toV66
// convention) so a half-applied run converges on re-run instead of wedging on
// "duplicate column". export_jobs is created in v67, so the table probe always
// passes on a real DB — it is defence for a minimally-seeded migration-heal DB.
func migrateV67toV68(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v67→v68 (add export_jobs.worker_conv_id): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'export_jobs'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v67→v68 (probe export_jobs): %w", err)
	}
	if haveTable > 0 {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('export_jobs') WHERE name = 'worker_conv_id'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v67→v68 (probe export_jobs.worker_conv_id): %w", err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE export_jobs ADD COLUMN worker_conv_id TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v67→v68 (add export_jobs.worker_conv_id): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 68`); err != nil {
		return fmt.Errorf("migrate v67→v68 (add export_jobs.worker_conv_id): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v67→v68 (add export_jobs.worker_conv_id): commit: %w", err)
	}
	return nil
}

// migrateV66toV67 adds export_jobs — the store behind the dashboard's
// per-agent "📋 summary…" export (JOH-265). A row is one request for a
// live agent to consolidate a shareable artifact: the daemon creates it
// (status='requested') and nudges the agent's pane; the agent fetches the
// brief (status flips to 'running'), produces its file(s), and uploads the
// result (status='ready'), or the job fails / times out (status='failed',
// with the reason in `error`).
//
// conv_id is the target agent's conversation; title / instructions / preset
// are the human's brief snapshotted at creation. The artifact_* columns are
// blank until upload: artifact_path is the on-disk file under
// ~/.tclaude/exports/<id>/, artifact_name the download filename, content_type
// its MIME type. created_at / updated_at are RFC3339Nano — ORDER on id, never
// these strings (the RFC3339Nano lexical-sort hazard, see ListHumanMessages).
//
// CREATE TABLE IF NOT EXISTS is idempotent, so a half-applied earlier run
// converges on re-run; the whole thing rides one transaction with the bump.
func migrateV66toV67(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS export_jobs (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			conv_id       TEXT NOT NULL,
			group_name    TEXT NOT NULL DEFAULT '',
			title         TEXT NOT NULL DEFAULT '',
			instructions  TEXT NOT NULL DEFAULT '',
			preset        TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL,
			error         TEXT NOT NULL DEFAULT '',
			artifact_path TEXT NOT NULL DEFAULT '',
			artifact_name TEXT NOT NULL DEFAULT '',
			artifact_size INTEGER NOT NULL DEFAULT 0,
			content_type  TEXT NOT NULL DEFAULT '',
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_export_jobs_conv
			ON export_jobs(conv_id);

		UPDATE schema_version SET version = 67;
	`); err != nil {
		return fmt.Errorf("migrate v66→v67 (add export_jobs): %w", err)
	}
	return nil
}

// migrateV65toV66 adds the tri-state remote-control DEFAULT/POLICY columns that
// let an operator arm Claude Code's built-in Remote Access at spawn without
// toggling each agent by hand (JOH-262):
//
//   - spawn_profiles.remote_control — a profile's "start with remote control"
//     default (NULL = unset, 0 = off, 1 = on), the tri-state *bool sibling of
//     the profile's auto_review / trust_dir toggles.
//   - agent_groups.remote_control — a group's remote-control policy that
//     OVERRIDES the profile default (NULL = inherit/unset, 0 = actively deny,
//     1 = actively opt-in).
//
// Both are NULLABLE (no NOT NULL / DEFAULT) so "unset" is a first-class state
// distinct from "off" — the precedence model needs all three. Resolution at
// spawn: group policy (if set) > profile default (if set) > off; the resolved
// intent feeds SpawnSpec.RemoteControl (JOH-258) and is what the relaunch
// re-arm (JOH-261) carries.
//
// One transaction; each ADD COLUMN is guarded by a pragma_table_info probe (the
// migrateV56toV57 convention) since SQLite has no ADD COLUMN IF NOT EXISTS, so a
// half-applied run converges on re-run instead of wedging on "duplicate column".
func migrateV65toV66(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v65→v66 (add remote-control defaults): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// table-name literals (never user input) so the pragma probe and ALTER use
	// the same string-literal convention as every prior column-add migration —
	// a bound `?` inside the table-valued pragma_table_info is version-fragile.
	// Each ALTER is guarded by BOTH a sqlite_master table-existence probe (the
	// migrateV61toV62 convention — a minimally-seeded migration-heal DB advancing
	// to head may not have created agent_groups / spawn_profiles, since a real DB
	// created them in earlier migrations) AND a pragma_table_info column probe (so
	// a half-applied run that already added the column converges instead of
	// wedging on "duplicate column"). A missing table means nothing to migrate.
	for _, probe := range []struct{ table, existsSQL, countSQL, alterSQL string }{
		{
			"spawn_profiles",
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profiles'`,
			`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'remote_control'`,
			`ALTER TABLE spawn_profiles ADD COLUMN remote_control INTEGER`,
		},
		{
			"agent_groups",
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_groups'`,
			`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'remote_control'`,
			`ALTER TABLE agent_groups ADD COLUMN remote_control INTEGER`,
		},
	} {
		var haveTable int
		if err := tx.QueryRow(probe.existsSQL).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v65→v66 (probe %s): %w", probe.table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveCol int
		if err := tx.QueryRow(probe.countSQL).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v65→v66 (add %s.remote_control): probe column: %w", probe.table, err)
		}
		if haveCol == 0 {
			// Nullable INTEGER (no NOT NULL / DEFAULT) so NULL means "unset",
			// distinct from 0 ("off") — the tri-state the policy needs.
			if _, err := tx.Exec(probe.alterSQL); err != nil {
				return fmt.Errorf("migrate v65→v66 (add %s.remote_control): add column: %w", probe.table, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 66`); err != nil {
		return fmt.Errorf("migrate v65→v66 (add remote-control defaults): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v65→v66 (add remote-control defaults): commit: %w", err)
	}
	return nil
}

// migrateV64toV65 adds sessions.remote_control — tclaude's best-known state
// of whether Claude Code's built-in Remote Access is ON for a live session
// (the /remote-control toggle / --remote-control launch flag). CC exposes no
// programmatic readback of remote-control state, so tclaude tracks it itself;
// the recorded flag decides whether the next toggle injection should enable
// (toggle) or disable (toggle + confirm Enter). It is a LIVE-session property
// (gone when the pane exits) like status, so it lives on sessions, defaults
// off, and is re-armed only by a --remote-control spawn. Written out-of-band
// (SetSessionRemoteControl), never by SaveSession's UPSERT, so hook ticks
// can't clobber it. See JOH-256.
//
// Runs in one transaction AND guards the column add behind a
// pragma_table_info probe (the migrateV56toV57 convention): SQLite has no
// ADD COLUMN IF NOT EXISTS, and a half-applied run must converge on re-run
// instead of wedging on "duplicate column name".
func migrateV64toV65(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v64→v65 (add sessions.remote_control): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveCol int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'remote_control'`,
	).Scan(&haveCol); err != nil {
		return fmt.Errorf("migrate v64→v65 (add sessions.remote_control): probe column: %w", err)
	}
	if haveCol == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE sessions ADD COLUMN remote_control INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migrate v64→v65 (add sessions.remote_control): add column: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 65`); err != nil {
		return fmt.Errorf("migrate v64→v65 (add sessions.remote_control): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v64→v65 (add sessions.remote_control): commit: %w", err)
	}
	return nil
}

// migrateV63toV64 adds ask_threads — the (terminal, cwd) → conversation map
// behind `tclaude ask` (project tclaude-ask, JOH-250). A terminal that asks
// repeated questions from the same directory continues one conversation
// instead of starting fresh each time; the row records which conv-id to
// `--resume`. Keyed on (term_key, cwd) because a Claude Code conversation is
// bound to its creation cwd (resume is cwd-scoped), and term_key scopes it to
// one terminal so two terminals in the same dir stay independent.
//
// CREATE TABLE IF NOT EXISTS is idempotent, so a half-applied earlier run
// converges on re-run (the migrateV55toV56 convention); the whole thing rides
// one transaction with the version bump.
func migrateV63toV64(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v63→v64 (add ask_threads): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS ask_threads (
			term_key   TEXT NOT NULL,
			cwd        TEXT NOT NULL,
			conv_id    TEXT NOT NULL,
			harness    TEXT NOT NULL DEFAULT 'claude',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (term_key, cwd)
		);

		UPDATE schema_version SET version = 64;
	`)
	if err != nil {
		return fmt.Errorf("migrate v63→v64 (add ask_threads): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v63→v64 (add ask_threads): commit: %w", err)
	}
	return nil
}

// migrateV62toV63 drops agent_groups.default_model — the Claude-only
// per-group spawn model (v52). JOH-210 inc2 replaced it with the
// harness-correct default_profile (v62) and stopped reading it at spawn,
// keeping the column vestigial only so the versioned group export/import
// format could round-trip it unchanged. With the export format bumped to
// drop it (groupexport.FormatVersion 2) and the import path synthesizing a
// default profile from any legacy default_model carried by an old archive,
// nothing reads or writes the column any more, so it is dropped to keep the
// schema honest (JOH-220).
//
// Guarded behind a pragma_table_info probe (the migrateV59toV60 / v56→v57
// convention): SQLite has no DROP COLUMN IF EXISTS, and a re-run (or a DB
// that somehow never had the column) must converge rather than wedge on
// "no such column". Tolerates a DB with no agent_groups table at all (a
// minimally-seeded migration-heal DB). Rides one transaction with the
// version bump.
func migrateV62toV63(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v62→v63 (drop agent_groups.default_model): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_groups'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v62→v63 (drop default_model): probe agent_groups: %w", err)
	}
	if haveTable > 0 {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_model'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v62→v63 (drop default_model): probe column: %w", err)
		}
		if haveCol > 0 {
			if _, err := tx.Exec(`ALTER TABLE agent_groups DROP COLUMN default_model`); err != nil {
				return fmt.Errorf("migrate v62→v63 (drop default_model): drop column: %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 63`); err != nil {
		return fmt.Errorf("migrate v62→v63 (drop default_model): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v62→v63 (drop default_model): commit: %w", err)
	}
	return nil
}

// migrateV61toV62 adds agent_groups.default_profile — the name of the spawn
// profile (spawn_profiles.name, v61) whose launch fields fill blank spawn
// fields server-side for agents spawned into the group (JOH-210 inc2). It is
// the harness-correct replacement for the Claude-only default_model column
// (v52): a profile carries its own harness and its model is validated against
// THAT harness's catalog at save, fixing the #343 bug where default_model was
// validated Claude-only (rejecting e.g. gpt-5) and then forwarded at spawn
// without revalidating against the spawn's harness.
//
// Existing per-group default_model values are migrated forward so nothing
// regresses: each group with a non-empty default_model gets a synthesized
// claude profile (named "group-default-<group>", suffixed on a name collision)
// carrying that model, and the group's default_profile is pointed at it. The
// vestigial default_model column is intentionally KEPT — still round-tripped
// by group export/import — but is no longer read at spawn; a follow-up PR drops
// it and bumps the export format.
//
// Idempotent / self-healing (the migrateV56toV57 convention): the ADD COLUMN
// is pragma_table_info-guarded, and the synthesis only touches groups whose
// default_profile is still ” (so a re-run after a half-applied attempt skips
// the groups it already converted). It also tolerates a DB with no agent_groups
// table / no default_model column (a minimally-seeded migration-heal DB). The
// whole thing rides one transaction with the version bump.
func migrateV61toV62(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v61→v62 (add agent_groups.default_profile): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Tolerate a DB that has no agent_groups table yet — a minimally-seeded
	// migration-heal DB advancing to head past versions that predate the table
	// (a real DB created agent_groups long before v61). Nothing to migrate, so
	// just land the version.
	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_groups'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v61→v62 (probe agent_groups): %w", err)
	}
	if haveTable > 0 {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_profile'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v61→v62 (add agent_groups.default_profile): probe column: %w", err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agent_groups ADD COLUMN default_profile TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v61→v62 (add agent_groups.default_profile): add column: %w", err)
			}
		}
		if err := synthesizeGroupDefaultProfiles(tx); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 62`); err != nil {
		return fmt.Errorf("migrate v61→v62 (add agent_groups.default_profile): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v61→v62 (add agent_groups.default_profile): commit: %w", err)
	}
	return nil
}

// synthesizeGroupDefaultProfiles migrates each group's legacy default_model
// into a synthesized claude spawn profile and points the group's default_profile
// at it, so the per-group spawn default survives the JOH-210 cutover. Only
// groups not yet converted (default_profile still ”) are touched, which makes
// it converge on a re-run. A DB whose agent_groups predates the v52
// default_model column (a minimally-seeded heal DB) has nothing to migrate, so
// the absence of default_model is tolerated.
func synthesizeGroupDefaultProfiles(tx *sql.Tx) error {
	var haveModelCol int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'default_model'`,
	).Scan(&haveModelCol); err != nil {
		return fmt.Errorf("migrate v61→v62 (synthesize profiles): probe default_model: %w", err)
	}
	if haveModelCol == 0 {
		return nil
	}

	// Collect the rows fully before the insert loop — the same tx cannot
	// interleave an open query with the writes below.
	rows, err := tx.Query(
		`SELECT name, default_model FROM agent_groups WHERE default_model != '' AND default_profile = ''`)
	if err != nil {
		return fmt.Errorf("migrate v61→v62 (synthesize profiles): query groups: %w", err)
	}
	type groupModel struct{ name, model string }
	var pending []groupModel
	for rows.Next() {
		var gm groupModel
		if err := rows.Scan(&gm.name, &gm.model); err != nil {
			_ = rows.Close()
			return fmt.Errorf("migrate v61→v62 (synthesize profiles): scan group: %w", err)
		}
		pending = append(pending, gm)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("migrate v61→v62 (synthesize profiles): iterate groups: %w", err)
	}
	_ = rows.Close()

	now := time.Now().Format(time.RFC3339Nano)
	for _, gm := range pending {
		profileName, err := uniqueSpawnProfileName(tx, "group-default-"+gm.name)
		if err != nil {
			return fmt.Errorf("migrate v61→v62 (synthesize profiles): pick name for group %q: %w", gm.name, err)
		}
		// harness 'claude': the legacy default_model passed the Claude-only
		// ValidateModel gate, so it is a Claude model by construction. Only
		// name/harness/model are set; every other field stays unset (its
		// column default — "" or NULL).
		if _, err := tx.Exec(
			`INSERT INTO spawn_profiles (name, harness, model, created_at, updated_at)
			 VALUES (?, 'claude', ?, ?, ?)`,
			profileName, gm.model, now, now); err != nil {
			return fmt.Errorf("migrate v61→v62 (synthesize profiles): insert profile for group %q: %w", gm.name, err)
		}
		if _, err := tx.Exec(
			`UPDATE agent_groups SET default_profile = ? WHERE name = ?`,
			profileName, gm.name); err != nil {
			return fmt.Errorf("migrate v61→v62 (synthesize profiles): point group %q at profile: %w", gm.name, err)
		}
	}
	return nil
}

// uniqueSpawnProfileName returns base, or base with a "-2"/"-3"/… suffix, such
// that it does not collide with an existing spawn_profiles.name. The v62
// migration uses it to synthesize per-group default profiles without tripping
// the UNIQUE(name) constraint when a human-made profile already holds the base
// name. Reads through the supplied tx so it sees rows inserted earlier in the
// same migration run.
func uniqueSpawnProfileName(tx *sql.Tx, base string) (string, error) {
	name := base
	for i := 2; ; i++ {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM spawn_profiles WHERE name = ?`, name).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// migrateV60toV61 adds spawn_profiles — the store behind reusable Spawn
// Profiles (JOH-210). A profile is a named, saved bundle of the dashboard's
// spawn-agent dialog (most fields, NOT cwd / worktree): pressing Spawn in a
// group with a default profile pre-fills the dialog from it, and the daemon
// resolves a group's default profile server-side to fill blank LAUNCH fields
// for non-dialog spawns (group templates) — replacing the per-group
// `default_model` column and its Claude-only validation/inheritance (#343).
//
// Each profile field is OPTIONAL — an unset field loads blank / leaves the
// launch default. Text fields use "" for unset. The five toggles
// (auto_review, trust_dir, sync_worktree, auto_focus,
// include_group_default_context) are NULLABLE so the schema can tell "unset"
// (NULL → leave the dialog's own default) from an explicit off (0) or on (1);
// the Go layer maps that to a *bool.
//
// CREATE TABLE IF NOT EXISTS is idempotent, so a half-applied earlier run
// converges on re-run (the migrateV55toV56 convention); the whole thing rides
// one transaction with the version bump.
func migrateV60toV61(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v60→v61 (add spawn_profiles): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS spawn_profiles (
			id                            INTEGER PRIMARY KEY AUTOINCREMENT,
			name                          TEXT NOT NULL UNIQUE,
			harness                       TEXT NOT NULL DEFAULT '',
			model                         TEXT NOT NULL DEFAULT '',
			effort                        TEXT NOT NULL DEFAULT '',
			sandbox                       TEXT NOT NULL DEFAULT '',
			approval                      TEXT NOT NULL DEFAULT '',
			auto_review                   INTEGER,
			trust_dir                     INTEGER,
			agent_name                    TEXT NOT NULL DEFAULT '',
			role                          TEXT NOT NULL DEFAULT '',
			descr                         TEXT NOT NULL DEFAULT '',
			initial_message               TEXT NOT NULL DEFAULT '',
			sync_worktree                 INTEGER,
			auto_focus                    INTEGER,
			include_group_default_context INTEGER,
			created_at                    TEXT NOT NULL,
			updated_at                    TEXT NOT NULL
		);

		UPDATE schema_version SET version = 61;
	`)
	if err != nil {
		return fmt.Errorf("migrate v60→v61 (add spawn_profiles): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v60→v61 (add spawn_profiles): commit: %w", err)
	}
	return nil
}

// migrateV59toV60 drops sessions.compact_pending — the bookkeeping flag
// of the removed auto-compact feature. Auto-compact CAS-claimed this
// column then injected `/compact` into the pane on Stop; the feature was
// removed because that injection fired at confusing moments across both
// harnesses. Nothing reads or writes the column any more, so it is
// dropped to keep the schema honest.
//
// Guarded behind a pragma_table_info probe (the migrateV56toV57
// convention): SQLite has no DROP COLUMN IF EXISTS, and a re-run (or a
// DB that somehow never had the column) must converge rather than wedge
// on "no such column". Rides one transaction with the version bump.
func migrateV59toV60(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v59→v60 (drop compact_pending): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveCol int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'compact_pending'`,
	).Scan(&haveCol); err != nil {
		return fmt.Errorf("migrate v59→v60 (drop compact_pending): probe column: %w", err)
	}
	if haveCol > 0 {
		if _, err := tx.Exec(`ALTER TABLE sessions DROP COLUMN compact_pending`); err != nil {
			return fmt.Errorf("migrate v59→v60 (drop compact_pending): drop column: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 60`); err != nil {
		return fmt.Errorf("migrate v59→v60 (drop compact_pending): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v59→v60 (drop compact_pending): commit: %w", err)
	}
	return nil
}

// migrateV58toV59 adds pending_spawns — the durable record of a dashboard
// spawn whose conv-id has not materialised yet (JOH-205 inc2). A Codex
// agent generates its conv-id at launch but only persists/exposes it after
// its first turn; an unattended pane stuck behind a startup gate (untrusted
// dir, a new-hooks-config prompt, the OpenAI auth modal) never takes that
// turn, so executeSpawn cannot resolve the conv-id synchronously. Rather
// than hang the request or orphan the pane, the dashboard spawn records its
// full enrollment intent here keyed by spawn label, returns a PENDING agent
// the operator can find + focus to clear the gate, and a sweeper back-fills
// the enrollment once the conv-id appears.
//
// The row carries everything finishSpawnEnrollment needs to complete the
// enrollment later WITHOUT the original request in memory — restart-safe:
// the group (group_id), the display/role/descr, the briefing inputs
// (initial_message, group_context, reply_to_conv), the spawner attribution
// (spawned_by_conv), and the worktree pair the welcome line references.
// label is the spawn label, which is also the session-row id, so the
// sweeper resolves the conv-id via LoadSession(label).
//
// CREATE TABLE IF NOT EXISTS is idempotent, so a half-applied earlier run
// converges on re-run (the migrateV55toV56 convention); the whole thing
// rides one transaction with the version bump.
func migrateV58toV59(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v58→v59 (add pending_spawns): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS pending_spawns (
			label           TEXT PRIMARY KEY,
			group_id        INTEGER NOT NULL,
			role            TEXT NOT NULL DEFAULT '',
			descr           TEXT NOT NULL DEFAULT '',
			name            TEXT NOT NULL DEFAULT '',
			initial_message TEXT NOT NULL DEFAULT '',
			group_context   TEXT NOT NULL DEFAULT '',
			reply_to_conv   TEXT NOT NULL DEFAULT '',
			spawned_by_conv TEXT NOT NULL DEFAULT '',
			worktree_path   TEXT NOT NULL DEFAULT '',
			worktree_branch TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL
		);

		UPDATE schema_version SET version = 59;
	`)
	if err != nil {
		return fmt.Errorf("migrate v58→v59 (add pending_spawns): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v58→v59 (add pending_spawns): commit: %w", err)
	}
	return nil
}

// migrateV57toV58 adds the `sandbox_mode` column to `sessions` — the
// launch-time OS-sandbox mode a session was spawned under (Codex's
// `--sandbox`: read-only / workspace-write / danger-full-access). Default
// "" so every existing row, and every reader that doesn't yet select the
// column, keeps working untouched; "" is also the genuine value for a
// harness with no launch sandbox flag (Claude Code, whose sandbox is
// settings.json-driven, not a launch flag). The dashboard reads it to
// render a per-agent sandbox badge (JOH-162).
//
// Unlike harness (v56→v57) this lands on `sessions` only: the sandbox mode
// is a property of a launched process, not of a stored conversation, so
// there is no conv_index counterpart. It is written once at spawn by
// `session new` (SaveSession owns the column) and is never re-derivable
// from the harness's own files, so it has no self-healing rescan path —
// the DEFAULT covers pre-migration rows, which simply show no badge.
//
// Single transaction, pragma_table_info-guarded (the migrateV56toV57
// convention): SQLite has no ADD COLUMN IF NOT EXISTS, so a half-applied /
// re-run must converge instead of wedging on "duplicate column name".
func migrateV57toV58(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v57→v58 (add sandbox_mode column): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveCol int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'sandbox_mode'`,
	).Scan(&haveCol); err != nil {
		return fmt.Errorf("migrate v57→v58 (add sessions.sandbox_mode): probe column: %w", err)
	}
	if haveCol == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE sessions ADD COLUMN sandbox_mode TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migrate v57→v58 (add sessions.sandbox_mode): add column: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 58`); err != nil {
		return fmt.Errorf("migrate v57→v58 (add sandbox_mode column): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v57→v58 (add sandbox_mode column): commit: %w", err)
	}
	return nil
}

// migrateV56toV57 adds the `harness` column to both `sessions` and
// `conv_index` — the harness (coding tool) each row belongs to. Default
// 'claude' so every existing row, and every reader that doesn't yet
// select the column, keeps working untouched; the Codex scan path
// (JOH-152+) writes 'codex' instead. The columns are filled on rescan by
// the scanner + UpsertConvIndex/SaveSession (self-healing), not by a
// one-shot backfill — DEFAULT covers the rows nothing has rescanned yet.
//
// Runs in one transaction AND guards each column add behind a
// pragma_table_info probe (the migrateV54toV55 convention): SQLite has no
// ADD COLUMN IF NOT EXISTS, and a half-applied run (e.g. interrupted
// between the two ALTERs) must converge on re-run instead of wedging on
// "duplicate column name". Each ALTER is probed independently so a run
// that added `sessions.harness` but not `conv_index.harness` heals.
func migrateV56toV57(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v56→v57 (add harness columns): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"sessions", "conv_index"} {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'harness'`,
			table,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v56→v57 (add %s.harness): probe column: %w", table, err)
		}
		if haveCol == 0 {
			// Table name is from a hardcoded loop, not user input, so the
			// Sprintf into the DDL (ALTER TABLE has no parameter binding
			// for identifiers) is safe.
			if _, err := tx.Exec(fmt.Sprintf(
				`ALTER TABLE %s ADD COLUMN harness TEXT NOT NULL DEFAULT 'claude'`, table,
			)); err != nil {
				return fmt.Errorf("migrate v56→v57 (add %s.harness): add column: %w", table, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 57`); err != nil {
		return fmt.Errorf("migrate v56→v57 (add harness columns): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v56→v57 (add harness columns): commit: %w", err)
	}
	return nil
}

// migrateV55toV56 adds dashboard_prefs — a flat key→value store for the
// browser dashboard's "sticky" view/config preferences (group
// expand/collapse, per-tab filters and toggles, the sort state, the
// spawn-modal auto-focus checkbox, and the per-model spawn effort
// memory). These used to live in the browser's localStorage, but the
// dashboard is served on a RANDOM loopback port each daemon start and
// localStorage is partitioned by origin (scheme+host+port) — so every
// such setting silently reset on restart. Moving them server-side
// makes them survive restarts, browser profiles and multiple tabs (the
// same reasoning the slop volume sliders already use, via config.json).
//
// Values are stored verbatim as the opaque strings the dashboard wrote
// to localStorage ('1'/'0', filter text, a JSON blob) — the daemon
// never looks inside them, so a flat TEXT KV is all that's needed; no
// JSON/JSONB column type buys anything here.
//
// CREATE TABLE IF NOT EXISTS is idempotent, so a half-applied earlier
// run converges on re-run (the migrateV53toV54 convention); the whole
// thing rides one transaction with the version bump.
func migrateV55toV56(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v55→v56 (add dashboard_prefs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS dashboard_prefs (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

		UPDATE schema_version SET version = 56;
	`)
	if err != nil {
		return fmt.Errorf("migrate v55→v56 (add dashboard_prefs): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v55→v56 (add dashboard_prefs): commit: %w", err)
	}
	return nil
}

// migrateV54toV55 adds sessions.model_id — the full Claude model ID
// (e.g. "claude-fable-5") the session last reported running on, from
// the statusline's model.id field. Sibling to sessions.model (v47),
// which stores the human-facing display name ("Fable 5") that can't
// be fed back to `claude --model`. The ID can: it's what lets a
// reincarnated / cloned / resumed agent come back on the SAME model
// its predecessor was running instead of claude's default
// (inheritedLaunchFlags in agentd reads it). "" = not reported yet —
// successor spawns then omit --model, the pre-v55 behaviour.
//
// Runs in one transaction AND guards the column add behind a
// pragma_table_info probe (the migrateV53toV54 convention): SQLite has
// no ADD COLUMN IF NOT EXISTS, and a half-applied run must converge on
// re-run instead of wedging on "duplicate column name".
func migrateV54toV55(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v54→v55 (add sessions.model_id): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveCol int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'model_id'`,
	).Scan(&haveCol); err != nil {
		return fmt.Errorf("migrate v54→v55 (add sessions.model_id): probe column: %w", err)
	}
	if haveCol == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE sessions ADD COLUMN model_id TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migrate v54→v55 (add sessions.model_id): add column: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 55`); err != nil {
		return fmt.Errorf("migrate v54→v55 (add sessions.model_id): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v54→v55 (add sessions.model_id): commit: %w", err)
	}
	return nil
}

// migrateV53toV54 adds the notification-filter knobs:
//
//   - agent_groups.notify_enabled — per-group OS-notification switch
//     (1 = notify, the historical behaviour). Muting a group silences
//     state-transition notifications for every member agent.
//   - agent_notify_prefs — per-agent tri-state override keyed by
//     conv-id: 'off' silences the agent, 'on' forces notifications
//     even when a containing group is muted, no row = inherit from
//     the group/global level.
//
// Both default to "everything notifies" so existing setups see no
// behaviour change.
//
// Runs in one transaction (the migrateV50toV51 convention) AND guards
// the column add: SQLite has no ALTER TABLE ... ADD COLUMN IF NOT
// EXISTS, and a half-applied earlier attempt (a pre-merge build of the
// non-transactional cut, or a write interrupted between statements)
// can leave the column behind with schema_version still 53 — at which
// point a bare re-run fails on "duplicate column name" forever, and
// the whole DB (groups tab included) is wedged behind the failing
// migrate. Probing pragma_table_info first makes the re-run converge
// instead: skip the ALTER, let the IF-NOT-EXISTS / UPDATE finish the
// job, and the DB self-heals on the next start.
func migrateV53toV54(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v53→v54 (notification filters): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveCol int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'notify_enabled'`,
	).Scan(&haveCol); err != nil {
		return fmt.Errorf("migrate v53→v54 (notification filters): probe column: %w", err)
	}
	if haveCol == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE agent_groups ADD COLUMN notify_enabled INTEGER NOT NULL DEFAULT 1`,
		); err != nil {
			return fmt.Errorf("migrate v53→v54 (notification filters): add column: %w", err)
		}
	}

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_notify_prefs (
			conv_id    TEXT PRIMARY KEY,
			mode       TEXT NOT NULL CHECK (mode IN ('on', 'off')),
			updated_at TEXT NOT NULL
		);

		UPDATE schema_version SET version = 54;
	`)
	if err != nil {
		return fmt.Errorf("migrate v53→v54 (notification filters): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v53→v54 (notification filters): commit: %w", err)
	}
	return nil
}

// migrateV52toV53 adds session_cost_daily.updated_at — the wall-clock
// time (RFC3339Nano, local) of the most recent spend recorded on that
// (session, day) row, so the Costs tab's per-agent breakdown can show
// and sort on a real last-activity time, not just the calendar day.
// Empty = unknown, which renders date-only (the prior behaviour).
//
// Going forward UpdateSessionCost stamps this when a day's cumulative
// figure actually rises. For rows that already exist we backfill from
// the sessions table — last_hook when it carries a real timestamp
// (the same per-session clock the Activity tab's "last activity"
// reads), else updated_at — but only where the session row still
// exists; history whose session was deleted keeps the empty value and
// stays date-only. The year guard ('… > 2000') skips the zero-time
// "0001-01-01…" that SaveSession writes for a never-hooked session.
func migrateV52toV53(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE session_cost_daily ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';

		UPDATE session_cost_daily SET updated_at = (
			SELECT CASE WHEN s.last_hook > '2000' THEN s.last_hook ELSE s.updated_at END
			FROM sessions s WHERE s.id = session_cost_daily.session_id
		)
		WHERE session_id IN (SELECT id FROM sessions);

		UPDATE schema_version SET version = 53;
	`)
	if err != nil {
		return fmt.Errorf("migrate v52→v53 (add session_cost_daily.updated_at): %w", err)
	}
	return nil
}

// migrateV51toV52 adds agent_groups.default_model — the Claude model
// alias (or full model ID) substituted into a spawn request that
// leaves model blank, so a group can run its whole team on e.g.
// "sonnet" without every spawn surface having to say so. Sibling to
// default_cwd (v27) / default_context (v29): an optional per-group
// spawn default, ” = unset (spawns then omit --model and claude
// falls back to the user-level settings.json model, then its own
// default).
func migrateV51toV52(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE agent_groups ADD COLUMN default_model TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 52;
	`)
	if err != nil {
		return fmt.Errorf("migrate v51→v52 (add agent_groups.default_model): %w", err)
	}
	return nil
}

// migrateV50toV51 adds session_cost_daily — a per-day snapshot of each
// session's cumulative API cost, written by the statusline hook
// alongside sessions.cost_usd (v50). One row per (session, local day)
// holding the highest cumulative figure seen that day, plus the
// session's conv_id denormalised in at write time so cost history
// survives the sessions row being deleted (session kill, agent
// delete) — the whole point of the table is that the Costs tab keeps
// showing what retired agents spent. A day's spend is recovered as
// the delta against the session's previous day's row.
//
// The backfill seeds today's row from every sessions row that already
// carries cost, so the daily series starts agreeing with the existing
// month-to-date sum immediately instead of waiting for each session's
// next statusline tick — and retired sessions, which will never tick
// again, aren't silently dropped from the new table. Cost accrued
// before this migration therefore all lands on the migration day.
//
// The whole migration runs in one transaction, with IF NOT EXISTS /
// OR IGNORE guards on top: an interrupted run must not strand the DB
// in a half-migrated state where schema_version is still 50 but the
// table already exists — every later startup would then fail on the
// bare CREATE TABLE.
func migrateV50toV51(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v50→v51: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS session_cost_daily (
			session_id TEXT NOT NULL,
			day        TEXT NOT NULL,
			conv_id    TEXT NOT NULL DEFAULT '',
			cost_usd   REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, day)
		);
		CREATE INDEX IF NOT EXISTS idx_session_cost_daily_day ON session_cost_daily(day);

		INSERT OR IGNORE INTO session_cost_daily (session_id, day, conv_id, cost_usd)
			SELECT id, date('now', 'localtime'), conv_id, cost_usd
			FROM sessions WHERE cost_usd > 0;

		UPDATE schema_version SET version = 51;
	`)
	if err != nil {
		return fmt.Errorf("migrate v50→v51 (add session_cost_daily): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v50→v51: commit: %w", err)
	}
	return nil
}

// migrateV49toV50 adds sessions.cost_usd — the session's cumulative API
// cost in USD (Claude Code's cost.total_cost_usd), recorded by the
// statusline hook ONLY when the session runs on API/enterprise pricing.
// On a subscription plan the statusline carries rate-limit buckets and
// the hook never writes cost, so the column stays 0 — which every
// surface (dashboard status column, statusbar) treats as "no cost data,
// render nothing". A sibling column to sessions.model (v47) and
// sessions.effort_level (v48): a single display-only value written on
// the statusline cadence, read back via GetContextSnapshot.
//
// Defaults to 0 — rows from before this migration, subscription-plan
// sessions, and sessions whose statusbar hasn't ticked yet all read
// back "no cost data".
func migrateV49toV50(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 50;
	`)
	if err != nil {
		return fmt.Errorf("migrate v49→v50 (add sessions.cost_usd): %w", err)
	}
	return nil
}

// migrateV47toV48 adds sessions.effort_level — Claude Code's live
// reasoning-effort level ("low", "medium", "high", "xhigh", "max") the
// agent is currently running on. Claude Code's statusline JSON carries
// it as effort.level on every render (when the model supports the
// reasoning-effort parameter), so the statusbar hook records it onto the
// session row alongside the model and context-window snapshot. The
// statusbar renders it left of the branch and the dashboard appends it
// to the per-agent model line ("CC · O4.8 1M high").
//
// A sibling column to sessions.model (v47) rather than a JSON side-blob:
// it's a single display-only string written on the same cadence as the
// model, so a plain column keeps it type-safe and consistent with how
// the model is already stored and surfaced.
//
// Defaults to ” — rows from before this migration, sessions whose
// statusbar hasn't ticked yet, and models without reasoning-effort
// support all read back empty, which both surfaces render as "no effort
// token" (the statusbar omits it; the dashboard line shows just the
// model).
func migrateV47toV48(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN effort_level TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 48;
	`)
	if err != nil {
		return fmt.Errorf("migrate v47→v48 (add sessions.effort_level): %w", err)
	}
	return nil
}

// migrateV48toV49 adds sessions.pending_conv — the conv-id a
// SessionStart hook with a transition source (clear / resume / compact)
// announced as this env-keyed session's NEXT conversation, recorded
// before the row's conv_id actually advances.
//
// Why it exists: hook callbacks key on TCLAUDE_SESSION_ID, which every
// subprocess of the session's pane inherits — including one-shot
// headless claude runs (`claude -p`, `claude mcp get`, …) an agent
// executes via its Bash tool. Those children fire hooks carrying their
// own throwaway conv-ids against the PARENT's session row, which the
// conv-rotation logic used to read as a /clear and migrate the agent's
// identity onto the throwaway conv (observed in production: a live
// agent retired as "superseded by <conv> (clear)" where <conv> was a
// 2-second plugin probe). pending_conv lets the hook callback tell the
// two apart: a mismatched conv-id is honoured only when a transition
// SessionStart announced it; anything else is a foreign process's
// event and is ignored.
//
// Defaults to ” — no announced transition. Overwritten by each new
// announcement; never read once the row's conv_id has advanced past it
// (conv-ids are UUIDs, so a stale value can't collide with a future
// foreign conv).
func migrateV48toV49(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN pending_conv TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 49;
	`)
	if err != nil {
		return fmt.Errorf("migrate v48→v49 (add sessions.pending_conv): %w", err)
	}
	return nil
}

// migrateV46toV47 adds sessions.model — the LLM model display name
// ("Opus 4.8", "Sonnet 4.6", …) the agent is currently running on.
// Claude Code's statusline JSON carries it (model.display_name) on
// every render, so the statusbar hook (`tclaude status-bar`) records
// it onto the session row alongside the context-window snapshot. The
// dashboard surfaces it per-agent so you can see at a glance which
// model each agent is on. Distinct from the context-snapshot columns
// only in that it's written unconditionally (the model is present in
// every render, including before a turn's first API response), not
// gated on the all-zero context guard.
//
// Defaults to ” — rows from before this migration, or sessions whose
// statusbar hasn't ticked yet, read back empty, which the dashboard
// renders as "model not reported yet" (no harness line).
func migrateV46toV47(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 47;
	`)
	if err != nil {
		return fmt.Errorf("migrate v46→v47 (add sessions.model): %w", err)
	}
	return nil
}

// migrateV45toV46 adds agent_workspace — the live "where the agent is
// right now" snapshot the statusbar (`tclaude status-bar`) refreshes on
// every Claude Code render. Distinct from agent_workdir (PostToolUse-
// driven, only updates on tool calls) and conv_index.git_branch (turn-
// driven, only updates when a .jsonl turn is appended): agent_workspace
// updates on CC's render cadence — independent of agent activity — so a
// plain `git checkout` in an idle session's launch dir reaches the
// dashboard within the next statusline render rather than after the
// next turn.
//
// One row per conv_id (PRIMARY KEY). Columns mirror what the statusbar
// already computes for its own display: cwd, branch, the GitHub repo
// URL, the repo's default branch, and the open PR's number/URL/state
// when one exists. updated_at is the freshness clock ResolveLocation
// uses to pick a winner across the three writers.
func migrateV45toV46(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_workspace (
			conv_id        TEXT PRIMARY KEY,
			cwd            TEXT NOT NULL DEFAULT '',
			branch         TEXT NOT NULL DEFAULT '',
			repo_url       TEXT NOT NULL DEFAULT '',
			default_branch TEXT NOT NULL DEFAULT '',
			pr_number      INTEGER NOT NULL DEFAULT 0,
			pr_url         TEXT NOT NULL DEFAULT '',
			pr_state       TEXT NOT NULL DEFAULT '',
			updated_at     TEXT NOT NULL DEFAULT ''
		);

		UPDATE schema_version SET version = 46;
	`); err != nil {
		return fmt.Errorf("migrate v45→v46 (add agent_workspace): %w", err)
	}
	return nil
}

// migrateV44toV45 adds conv_branch_history — the per-conversation set
// of git branches an agent has worked on, with an optional PR snapshot
// per branch. It is the store behind the (future) dashboard "branch
// history" affordance; this migration only lands the schema.
//
// Identity is (conv_id, repo_dir, branch): a single conversation can
// work across several repos, and a bare branch name collides between
// them (two repos each have a `main`; same-named feature branches
// happen too) — so the repo directory is part of the key. repo_dir is
// canonicalised (see db.CanonicalizeRepoDir) before every write so the
// scan path and the hook agree on one spelling. The PR is a mutable
// attribute of a row (none → open → merged), not part of the key.
//
// The `source` column records which path wrote the row — 'scan' for
// the idempotent .jsonl re-scan (the source of truth, which fully
// rebuilds its own rows), 'hook' for the cheap PostToolUse append that
// also catches branches in worktrees the launch-dir .jsonl never
// names. pr_* columns are a best-effort snapshot refreshed by the
// dashboard's branch-link resolver; an unresolved or PR-less branch
// keeps them at their zero values.
//
// No separate conv_id index: the primary key is left-prefixed by
// conv_id, so the by-conversation listing query already rides it.
func migrateV44toV45(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conv_branch_history (
			conv_id    TEXT NOT NULL,
			repo_dir   TEXT NOT NULL DEFAULT '',
			branch     TEXT NOT NULL,
			pr_number  INTEGER NOT NULL DEFAULT 0,
			pr_url     TEXT NOT NULL DEFAULT '',
			pr_state   TEXT NOT NULL DEFAULT '',
			source     TEXT NOT NULL DEFAULT 'scan',
			first_seen TEXT NOT NULL DEFAULT '',
			last_seen  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (conv_id, repo_dir, branch)
		);

		UPDATE schema_version SET version = 45;
	`); err != nil {
		return fmt.Errorf("migrate v44→v45 (add conv_branch_history): %w", err)
	}
	return nil
}

// migrateV43toV44 adds human_messages — the store behind the dashboard
// Messages tab, where a coordinating agent's notifications to the human
// land (POST /v1/notify-human).
//
// from_title and group_name are snapshots taken at insert time, not
// foreign keys: a later rename or deletion of the sending agent must
// not blank an old message. read_at is empty for an unread message and
// an RFC3339 timestamp once the human marks it read.
func migrateV43toV44(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS human_messages (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			from_conv   TEXT NOT NULL,
			from_title  TEXT NOT NULL DEFAULT '',
			group_name  TEXT NOT NULL DEFAULT '',
			subject     TEXT NOT NULL DEFAULT '',
			body        TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			read_at     TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_human_messages_created
			ON human_messages(created_at);

		UPDATE schema_version SET version = 44;
	`); err != nil {
		return fmt.Errorf("migrate v43→v44 (add human_messages): %w", err)
	}
	return nil
}

// migrateV41toV42 adds group_templates + group_template_agents — the
// storage behind the dashboard's group-template feature.
//
// A template is a reusable BLUEPRINT for a working group: a name, an
// optional shared startup context, and an ordered list of agent specs
// (name / role / descr / per-role task brief / owner flag / permission
// slugs). It is deliberately distinct from a group EXPORT (agent_*
// rows + .jsonl, a conv-bound snapshot of a live group): a template has
// no conv-ids — instantiating one creates a fresh group and spawns one
// new agent per spec.
//
// group_template_agents.permissions is a JSON array of permission
// slugs, granted to the agent as per-conv overrides right after it
// spawns. The agent rows cascade-delete with their template.
func migrateV41toV42(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS group_templates (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL UNIQUE,
			descr           TEXT NOT NULL DEFAULT '',
			default_context TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS group_template_agents (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			template_id     INTEGER NOT NULL
			                  REFERENCES group_templates(id) ON DELETE CASCADE,
			ordinal         INTEGER NOT NULL DEFAULT 0,
			name            TEXT NOT NULL,
			role            TEXT NOT NULL DEFAULT '',
			descr           TEXT NOT NULL DEFAULT '',
			initial_message TEXT NOT NULL DEFAULT '',
			is_owner        INTEGER NOT NULL DEFAULT 0,
			permissions     TEXT NOT NULL DEFAULT '[]'
		);
		CREATE INDEX IF NOT EXISTS idx_group_template_agents_template
			ON group_template_agents(template_id);

		UPDATE schema_version SET version = 42;
	`); err != nil {
		return fmt.Errorf("migrate v41→v42 (add group templates): %w", err)
	}
	return nil
}

// migrateV42toV43 adds sessions.exit_reason — the nullable column that
// lets the dashboard tell a clean exit from an unexpected death.
//
// When Claude Code shuts down gracefully it fires a SessionEnd hook
// carrying a `reason` (clear / logout / prompt_input_exit / resume /
// bypass_permissions_disabled / other); the hook callback records that
// reason here. A process that dies WITHOUT a graceful shutdown — a
// crash, an OOM kill, `tclaude session kill`, a reboot — fires no
// SessionEnd, so the session reaper can find a dead row carrying no
// recorded reason and stamp exit_reason='unexpected' when it marks
// the row exited (see MarkSessionExitedIfUnchanged). Harnesses without
// a reliable SessionEnd equivalent may leave the reason NULL instead.
//
// The column is nullable on purpose: NULL means "no reason recorded" —
// a live session, or a row that exited before this migration existed.
// The dashboard treats NULL as a plain offline/exited, never as a
// crash, so pre-migration corpses are not retroactively mislabelled.
// Only an explicit 'unexpected' renders as crashed.
func migrateV42toV43(db *sql.DB) error {
	if _, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN exit_reason TEXT;

		UPDATE schema_version SET version = 43;
	`); err != nil {
		return fmt.Errorf("migrate v42→v43 (add sessions.exit_reason): %w", err)
	}
	return nil
}

// migrateV40toV41 adds agent_cron_jobs.target_kind — the discriminator
// that lets a cron job target a whole GROUP, not just a single conv.
//
// Before this, a cron job had target_conv (the recipient) and group_id
// (the routing group through which a conv-targeted message is sent, or
// 0 for a direct send-keys). There was no way to say "fan this out to
// every member of group X" — the dashboard's Group (multicast) radio
// was dead UI.
//
// target_kind carries 'conv' or 'group':
//   - 'conv'  → target_conv is the recipient; group_id (when >0) is the
//     routing group. The long-standing behaviour.
//   - 'group' → group_id IS the target group; the scheduler resolves
//     that group's membership AT FIRE TIME and fans the body out to
//     every current member. target_conv is unused.
//
// Existing rows backfill to 'conv' — every cron job written before this
// migration was, by construction, conv-targeted. The CHECK constraint
// pins the column to the two legal values so a stray write can't leave
// a job the scheduler cannot classify.
func migrateV40toV41(db *sql.DB) error {
	if _, err := db.Exec(`
		ALTER TABLE agent_cron_jobs
			ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'conv'
			CHECK (target_kind IN ('conv', 'group'));

		UPDATE schema_version SET version = 41;
	`); err != nil {
		return fmt.Errorf("migrate v40→v41 (add agent_cron_jobs.target_kind): %w", err)
	}
	return nil
}

// migrateV39toV40 adds agent_transfer_log — the persistent audit trail
// for per-group export / import (tclaude agent groups export|import).
//
// Each row records one transfer: kind 'export' or 'import', when it
// happened, the export's format_version, and — for imports — the source
// machine bases the export was taken against (source_home / source_os),
// the resulting group name + target dir, the conv-id remaps that were
// applied (a JSON object of collided source-id → freshly minted id), and
// agent / message counts. The import handler writes its row INSIDE the
// import transaction, so a rolled-back import logs nothing — the log can
// never claim an import that did not land. Exports log a lighter row
// best-effort after the fact.
//
// A dedicated table rather than an extension of agent_group_audit:
// agent_group_audit is rename-specific (old_name/new_name/by_conv/at),
// whereas a transfer entry carries a different, richer shape. Keeping
// them separate keeps each table's columns meaningful.
func migrateV39toV40(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_transfer_log (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			kind           TEXT NOT NULL,
			at             TEXT NOT NULL,
			format_version INTEGER NOT NULL DEFAULT 0,
			source_group   TEXT NOT NULL DEFAULT '',
			source_home    TEXT NOT NULL DEFAULT '',
			source_os      TEXT NOT NULL DEFAULT '',
			result_group   TEXT NOT NULL DEFAULT '',
			target_dir     TEXT NOT NULL DEFAULT '',
			conv_remaps    TEXT NOT NULL DEFAULT '',
			agent_count    INTEGER NOT NULL DEFAULT 0,
			message_count  INTEGER NOT NULL DEFAULT 0,
			by_conv        TEXT NOT NULL DEFAULT '',
			note           TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_agent_transfer_log_at
			ON agent_transfer_log(at);

		UPDATE schema_version SET version = 40;
	`); err != nil {
		return fmt.Errorf("migrate v39→v40 (add agent_transfer_log): %w", err)
	}
	return nil
}

// migrateV38toV39 adds agent_permissions.effect — the grant/deny knob
// behind the dashboard's permanent-permission editor.
//
// Before this, agent_permissions held grants only: a row meant "this
// conv holds this slug," ADDED on top of the config.json defaults.
// There was no way to DENY a default-granted slug for one specific
// agent. The tri-state editor (Grant / Deny / Default) needs that, so
// the column carries 'grant' or 'deny'; the absence of a row is the
// third state, "inherit the default."
//
// Existing rows backfill to 'grant' — every per-conv row written
// before this migration was, by construction, an additive grant.
func migrateV38toV39(db *sql.DB) error {
	if _, err := db.Exec(`
		ALTER TABLE agent_permissions
			ADD COLUMN effect TEXT NOT NULL DEFAULT 'grant'
			CHECK (effect IN ('grant', 'deny'));

		UPDATE schema_version SET version = 39;
	`); err != nil {
		return fmt.Errorf("migrate v38→v39 (add agent_permissions.effect): %w", err)
	}
	return nil
}

// migrateV37toV38 sanitizes legacy group names containing a slash or
// backslash. Group create historically skipped validateGroupName, so a
// name like "team/sub" could be stored — and once stored, every
// /v1/groups/{name}/... and /api/groups/{name}/... route on that group
// became unroutable: the path dispatcher splits on "/", so the embedded
// slash re-split the name into bogus path segments. Create now rejects
// such names up front; this migration repairs any that already slipped
// through so the affected groups become operable (renameable, even)
// again on the next daemon start.
//
// Each offending name has its slashes (forward and back) folded to "-".
// agent_groups.name is UNIQUE, so a sanitized name that collides with an
// existing group gets a numeric "-2", "-3", … suffix. Group references
// are integer foreign keys (group_id), so each repair is a single-row
// UPDATE with no cascade across members, owners, messages, or cron jobs.
func migrateV37toV38(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v37→v38: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Snapshot every existing name first so collision resolution sees
	// both the rows still to be migrated and the names already taken.
	// ORDER BY id makes the suffixing deterministic — when two slashed
	// names fold to the same base, the lower (older) id keeps the bare
	// sanitized name and later ones take the numeric suffix.
	rows, err := tx.Query(`SELECT id, name FROM agent_groups ORDER BY id`)
	if err != nil {
		return fmt.Errorf("migrate v37→v38: scan groups: %w", err)
	}
	type groupRow struct {
		id   int64
		name string
	}
	var all []groupRow
	taken := map[string]bool{}
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.id, &g.name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("migrate v37→v38: scan row: %w", err)
		}
		all = append(all, g)
		taken[g.name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("migrate v37→v38: rows: %w", err)
	}
	_ = rows.Close()

	folder := strings.NewReplacer("/", "-", `\`, "-")
	for _, g := range all {
		if !strings.ContainsAny(g.name, `/\`) {
			continue
		}
		base := folder.Replace(g.name)
		if base == "" {
			base = "group"
		}
		candidate := base
		for n := 2; taken[candidate]; n++ {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		if _, err := tx.Exec(`UPDATE agent_groups SET name = ? WHERE id = ?`, candidate, g.id); err != nil {
			return fmt.Errorf("migrate v37→v38: rename group %d: %w", g.id, err)
		}
		delete(taken, g.name)
		taken[candidate] = true
		slog.Info("migrate v37→v38: sanitized group name with slash",
			"old", g.name, "new", candidate)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 38;`); err != nil {
		return fmt.Errorf("migrate v37→v38: version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v37→v38: commit: %w", err)
	}
	return nil
}

// migrateV36toV37 adds agent_enrollment.pending_name — the agent's
// intended display name, recorded at spawn time from the `tclaude agent
// spawn --name` argument.
//
// Why a dedicated column rather than reusing conv_index.custom_title:
// conv_index is a cache rebuilt from the conversation .jsonl on every
// mtime change, so a name written there would be clobbered back to ""
// by the first rescan (the fresh .jsonl has no custom-title turn until
// the agent's /rename lands). agent_enrollment is never touched by the
// .jsonl scan, so a pending name written here is stable — it survives
// every snapshot refresh until the real /rename supersedes it.
//
// Existing rows backfill to ” (no pending name) — they are agents that
// have long since been named, so the read path resolves their title
// from conv_index as before.
func migrateV36toV37(db *sql.DB) error {
	if _, err := db.Exec(`
		ALTER TABLE agent_enrollment
			ADD COLUMN pending_name TEXT NOT NULL DEFAULT '';

		UPDATE schema_version SET version = 37;
	`); err != nil {
		return fmt.Errorf("migrate v36→v37 (add agent_enrollment.pending_name): %w", err)
	}
	return nil
}

// migrateV35toV36 makes agent_messages.group_id optional. The column was
//
//	group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT
//
// which meant a message row could not exist without a shared routing
// group — so solo (groupless) agents had no inbox at all. The "universal
// inbox" change makes agent_messages the universal transport: group_id
// becomes `NOT NULL DEFAULT 0`, where 0 means "direct (no routing
// group)", and the foreign key is dropped (0 can never satisfy it, and
// the FK only ever served as the thing DeleteAgentGroup had to work
// around). Group membership becomes purely an authorisation policy, not a
// transport constraint.
//
// SQLite cannot ALTER a column's NOT NULL / FK in place, so this is the
// standard table rebuild: create the replacement, copy the rows, drop
// the original, rename. agent_messages is referenced by no other table
// (parent_id is a self-column with no declared FK), so a straight
// column-named copy is safe even with foreign_keys enforced — the
// "12-step" pragma dance is only needed when the rebuilt table is
// *referenced by* FKs elsewhere.
//
// The whole rebuild runs in one explicit transaction. That makes it
// atomic: a failure mid-rebuild rolls back cleanly, so there is never a
// partial state — no orphaned scratch table, and no window where
// agent_messages has been dropped but not yet replaced.
//
// The scratch table is named agent_messages_v2 — the convention being
// "<table>_vN for the Nth rebuild of that table", so a future second
// rebuild would use _v3 and the migration history reads unambiguously.
// It is renamed to agent_messages before the transaction commits and
// never outlives this function.
func migrateV35toV36(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v35→v36: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE agent_messages_v2 (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id         INTEGER NOT NULL DEFAULT 0,
			from_conv        TEXT NOT NULL,
			to_conv          TEXT NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL,
			delivered_at     TEXT NOT NULL DEFAULT '',
			read_at          TEXT NOT NULL DEFAULT '',
			parent_id        INTEGER NOT NULL DEFAULT 0,
			to_recipients    TEXT NOT NULL DEFAULT '',
			cc_recipients    TEXT NOT NULL DEFAULT '',
			original_to_conv TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO agent_messages_v2
			(id, group_id, from_conv, to_conv, subject, body, created_at,
			 delivered_at, read_at, parent_id, to_recipients, cc_recipients,
			 original_to_conv)
			SELECT id, group_id, from_conv, to_conv, subject, body, created_at,
			       delivered_at, read_at, parent_id, to_recipients, cc_recipients,
			       original_to_conv
			FROM agent_messages;
		DROP TABLE agent_messages;
		ALTER TABLE agent_messages_v2 RENAME TO agent_messages;
		CREATE INDEX IF NOT EXISTS idx_agent_messages_to_conv
			ON agent_messages(to_conv, created_at);
		CREATE INDEX IF NOT EXISTS idx_agent_messages_parent
			ON agent_messages(parent_id);

		UPDATE schema_version SET version = 36;
	`); err != nil {
		return fmt.Errorf("migrate v35→v36: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v35→v36: commit: %w", err)
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
// the member role/descr fields.
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
// tmux-probed from a SQL migration — the daemon's session reaper
// enrolls those from its continuous liveness sweep instead (see agentd
// enrollOnlineSession).
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
	// Source the conv UNION column-aware: after the JOH-26 cutovers the
	// membership/owner/permission/sudo tables (v73) and the clone history + cron
	// owner/target columns (v74) become agent-keyed (no conv_id / *_conv), so
	// their SELECT would otherwise fail with "no such column". Those convs are
	// already agents by then, so dropping them from the coverage UNION is
	// correct. backfillAgentEnrollment only runs at v30 in production, where they
	// still carry the conv column; the column guard just keeps it (and its test)
	// robust against the head schema.
	sources := []struct{ table, col string }{
		{"agent_group_members", "conv_id"},
		{"agent_group_owners", "conv_id"},
		{"agent_permissions", "conv_id"},
		{"agent_sudo_grants", "conv_id"},
		{"agent_head_aliases", "anchor_conv_id"},
		{"agent_conv_succession", "new_conv_id"},
		{"agent_clone_history", "source_conv_id"},
		{"agent_cron_jobs", "owner_conv"},
		{"agent_cron_jobs", "target_conv"},
		{"agent_workdir", "conv_id"},
		{"agent_messages", "from_conv"},
		{"agent_messages", "to_conv"},
	}
	var selects []string
	for _, s := range sources {
		ok, err := columnExists(db, s.table, s.col)
		if err != nil {
			return err
		}
		if ok {
			selects = append(selects, "SELECT "+s.col+" AS conv_id FROM "+s.table)
		}
	}
	if len(selects) == 0 {
		return nil
	}
	// Exclude superseded conversations (succession old_conv_id) only when the
	// succession table is present.
	exclude := ""
	if ok, err := tableExists(db, "agent_conv_succession"); err != nil {
		return err
	} else if ok {
		exclude = ` AND conv_id NOT IN (
			SELECT old_conv_id FROM agent_conv_succession
			WHERE old_conv_id IS NOT NULL AND old_conv_id != ''
		)`
	}
	now := time.Now().Format(time.RFC3339Nano)
	q := `INSERT OR IGNORE INTO agent_enrollment (conv_id, enrolled_at, enrolled_via)
		SELECT conv_id, ?, 'migration' FROM (` + strings.Join(selects, " UNION ") + `)
		WHERE conv_id IS NOT NULL AND conv_id != ''` + exclude
	_, err := db.Exec(q, now)
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
// memberships or installing owner bridges.
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

	// Move debug.log from the oldest location
	// (~/.tclaude/claude-sessions/debug.log) into the private data dir
	// (~/.tclaude/data/debug.log) before renaming the directory. This runs
	// during DB open, i.e. after the api/data split relocation, so its target
	// is the data/ subtree (denied to sandboxed agents), not the tclaude root.
	oldDebugLog := filepath.Join(home, ".tclaude", "claude-sessions", "debug.log")
	newDebugLog := filepath.Join(home, ".tclaude", "data", "debug.log")
	if _, err := os.Stat(oldDebugLog); err == nil {
		if _, err := os.Stat(newDebugLog); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(newDebugLog), 0o700); err != nil {
				slog.Warn("failed to create data dir for debug.log", "error", err)
			} else if err := os.Rename(oldDebugLog, newDebugLog); err != nil {
				slog.Warn("failed to move debug.log", "error", err)
			}
		}
	}

	if importedSessions {
		oldDir := filepath.Join(home, ".tclaude", "claude-sessions")
		newDir := filepath.Join(home, ".tclaude", "data", "claude-sessions.migrated")
		if err := os.Rename(oldDir, newDir); err != nil {
			slog.Warn("failed to rename legacy sessions dir", "error", err)
		}
	}
	if importedNotify {
		oldDir := filepath.Join(home, ".tclaude", "notify-state")
		newDir := filepath.Join(home, ".tclaude", "data", "notify-state.migrated")
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
