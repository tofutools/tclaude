package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV53toV54_AddsNotifyFilters seeds a bare v53 agent_groups
// table, runs the v54 migration, and asserts the notification-filter
// knobs land: existing groups default to notify_enabled = 1 (no
// behaviour change), and the agent_notify_prefs table exists with its
// mode CHECK constraint.
func TestMigrateV53toV54_AddsNotifyFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v53.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (53);
		CREATE TABLE agent_groups (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE,
			descr       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL
		);
		INSERT INTO agent_groups (name, descr, created_at) VALUES ('team', '', '2026-06-01T00:00:00Z');
	`)
	require.NoError(t, err, "seed v53 schema")

	require.NoError(t, migrateV53toV54(d), "migrateV53toV54")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 54, ver, "schema_version after migration")

	var enabled int
	require.NoError(t, d.QueryRow(`SELECT notify_enabled FROM agent_groups WHERE name = 'team'`).Scan(&enabled))
	assert.Equal(t, 1, enabled, "existing groups default to notifications on")

	_, err = d.Exec(`INSERT INTO agent_notify_prefs (conv_id, mode, updated_at) VALUES ('c1', 'off', '2026-06-01T00:00:00Z')`)
	assert.NoError(t, err, "agent_notify_prefs accepts a valid mode")
	_, err = d.Exec(`INSERT INTO agent_notify_prefs (conv_id, mode, updated_at) VALUES ('c2', 'bogus', '2026-06-01T00:00:00Z')`)
	assert.Error(t, err, "the mode CHECK rejects unknown values")
}

// TestMigrateV53toV54_FreshSchemaRoundTrips builds a fresh DB through
// the full migrate() chain and round-trips the group switch and the
// per-agent prefs through the production helpers. Carries the literal
// currentVersion pin — a tripwire the next migration's author moves
// forward into their own v55 test.
func TestMigrateV53toV54_FreshSchemaRoundTrips(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 54, currentVersion, "currentVersion is 54")

	// Group switch: on by default, flips off and back via the setter.
	_, err = CreateAgentGroup("team", "")
	require.NoError(t, err, "CreateAgentGroup")
	g, err := GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.True(t, g.NotifyEnabled, "a fresh group notifies by default")

	n, err := SetAgentGroupNotifyEnabled("team", false)
	require.NoError(t, err, "SetAgentGroupNotifyEnabled")
	assert.Equal(t, int64(1), n, "one row updated")
	g, err = GetAgentGroupByName("team")
	require.NoError(t, err)
	assert.False(t, g.NotifyEnabled, "mute persisted")

	n, err = SetAgentGroupNotifyEnabled("nope", false)
	require.NoError(t, err)
	assert.Zero(t, n, "unknown group updates zero rows (callers answer 404)")

	// Per-agent prefs: set / read / list / clear.
	require.NoError(t, SetConvNotifyPref("conv-1", NotifyPrefOff))
	mode, err := GetConvNotifyPref("conv-1")
	require.NoError(t, err)
	assert.Equal(t, NotifyPrefOff, mode)

	require.NoError(t, SetConvNotifyPref("conv-1", NotifyPrefOn), "upsert replaces the mode")
	mode, err = GetConvNotifyPref("conv-1")
	require.NoError(t, err)
	assert.Equal(t, NotifyPrefOn, mode)

	require.NoError(t, SetConvNotifyPref("conv-2", NotifyPrefOff))
	prefs, err := ListConvNotifyPrefs()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"conv-1": NotifyPrefOn, "conv-2": NotifyPrefOff}, prefs)

	require.NoError(t, SetConvNotifyPref("conv-1", NotifyPrefInherit), "inherit deletes the override")
	mode, err = GetConvNotifyPref("conv-1")
	require.NoError(t, err)
	assert.Empty(t, mode, "no override left")

	assert.Error(t, SetConvNotifyPref("conv-1", "bogus"), "unknown mode rejected")
}

// TestNotifyPref_FollowsAgentLifecycle asserts the pref rides the two
// lifecycle paths that rekey or purge conv-id-keyed identity rows:
// MigrateAgentIdentity (reincarnate / clear) carries it to the new
// conv-id, and DeleteAgentByConvID removes it.
func TestNotifyPref_FollowsAgentLifecycle(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SetConvNotifyPref("old-conv", NotifyPrefOff))
	mig, err := MigrateAgentIdentity("old-conv", "new-conv", "reincarnate", "system:test")
	require.NoError(t, err, "MigrateAgentIdentity")
	assert.Equal(t, int64(1), mig.NotifyPrefs, "one pref row rekeyed")

	mode, err := GetConvNotifyPref("new-conv")
	require.NoError(t, err)
	assert.Equal(t, NotifyPrefOff, mode, "the mute followed the agent")
	mode, err = GetConvNotifyPref("old-conv")
	require.NoError(t, err)
	assert.Empty(t, mode, "nothing left on the old conv")

	counts, err := DeleteAgentByConvID("new-conv")
	require.NoError(t, err, "DeleteAgentByConvID")
	assert.Equal(t, int64(1), counts.NotifyPrefs, "delete purges the pref row")
	mode, err = GetConvNotifyPref("new-conv")
	require.NoError(t, err)
	assert.Empty(t, mode)
}
