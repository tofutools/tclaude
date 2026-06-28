package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV81toV82_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. v82 is head, so the literal
// currentVersion tripwire lives here now (moved forward from v81).
func TestMigrateV81toV82_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 82, currentVersion, "tripwire: bump this and add a v82→v83 test when you add a migration")
}

// TestMigrateV81toV82_BackfillsArchivedAt drives the real v81→v82 backfill over
// a v81-pinned DB seeded with conv_index + agent_conv_succession rows. It
// asserts the column is stamped on reincarnation predecessors (old_conv_id of a
// succession edge) and nothing else, that an existing manual archive timestamp
// survives, that the version advances, and that a re-run is a clean no-op.
func TestMigrateV81toV82_BackfillsArchivedAt(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v81 and seed the pre-backfill state.
	mustExec(t, d, `UPDATE schema_version SET version = 81`)

	// conv_index rows:
	//   pred      — a reincarnate predecessor, active → must be archived
	//   head      — only ever a successor (new_conv_id) → must stay active
	//   live-x    — not in any succession edge, active → must stay active
	//   manual    — a reincarnate predecessor already manually archived → untouched
	//   cleared   — a /clear predecessor, active → must STAY active (not -x-renamed,
	//               forward /clear doesn't stamp the column; backfill must match)
	mustExec(t, d, `INSERT INTO conv_index (conv_id, project_dir, full_path, archived_at) VALUES
		('pred',    '/p', '/p/pred.jsonl',    ''),
		('head',    '/p', '/p/head.jsonl',    ''),
		('live-x',  '/p', '/p/live-x.jsonl',  ''),
		('manual',  '/p', '/p/manual.jsonl',  '2020-01-01T00:00:00Z'),
		('cleared', '/p', '/p/cleared.jsonl', '')`)

	// succession edges. reason='reincarnate' for the real predecessors;
	// orphan→head3 has NO conv_index row (silent no-op); cleared→head4 is a
	// /clear rotation (reason='clear') that must be left visible.
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at) VALUES
		('pred',    'head',  'reincarnate', '2026-05-01T10:00:00Z'),
		('manual',  'head2', 'reincarnate', '2026-05-02T10:00:00Z'),
		('orphan',  'head3', 'reincarnate', '2026-05-03T10:00:00Z'),
		('cleared', 'head4', 'clear',       '2026-05-04T10:00:00Z')`)

	require.NoError(t, migrateV81toV82(d), "v81→v82")

	archivedAt := func(convID string) string {
		var v string
		require.NoError(t, d.QueryRow(`SELECT archived_at FROM conv_index WHERE conv_id = ?`, convID).Scan(&v))
		return v
	}

	assert.Equal(t, "2026-05-01T10:00:00Z", archivedAt("pred"), "reincarnate predecessor stamped from succeeded_at")
	assert.Equal(t, "", archivedAt("head"), "successor head stays active")
	assert.Equal(t, "", archivedAt("live-x"), "non-predecessor stays active (JOH-320: -x title no longer hides)")
	assert.Equal(t, "2020-01-01T00:00:00Z", archivedAt("manual"), "existing manual archive timestamp preserved")
	assert.Equal(t, "", archivedAt("cleared"), "/clear predecessor stays visible (backfill scoped to reason=reincarnate)")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 82, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB changes nothing.
	require.NoError(t, migrateV81toV82(d), "v81→v82 re-run is a clean no-op")
	assert.Equal(t, "2026-05-01T10:00:00Z", archivedAt("pred"), "re-run leaves the stamp unchanged")
	assert.Equal(t, "", archivedAt("live-x"), "re-run still leaves non-predecessors active")
}
