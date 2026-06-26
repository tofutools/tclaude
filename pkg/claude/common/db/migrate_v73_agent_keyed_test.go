package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV72toV73_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts the authz/identity tables came out agent-keyed. v73 is
// head, so the literal currentVersion tripwire lives here now (moved forward
// from the v72 test); the next migration's author moves it into their own test.
func TestMigrateV72toV73_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	// The cutover swapped conv_id for agent_id on every identity table.
	for _, tbl := range []string{
		"agent_group_members", "agent_group_owners",
		"agent_permissions", "agent_sudo_grants", "agent_notify_prefs",
	} {
		hasAgent, err := columnExists(d, tbl, "agent_id")
		require.NoError(t, err)
		assert.True(t, hasAgent, "%s is agent-keyed", tbl)
		hasConv, err := columnExists(d, tbl, "conv_id")
		require.NoError(t, err)
		assert.False(t, hasConv, "%s no longer carries conv_id", tbl)
	}
}

// TestMigrateV72toV73_CollapsesGenerationsDeterministically drives the real
// v72→v73 cutover over a two-generation actor (old → new, a reincarnation
// chain). Both generations carried a row for the same group / slug under the
// old conv-keyed schema; after the cutover they collapse to ONE agent-keyed row
// — and the collapse is deterministic: newest wins for the membership role,
// DENY wins for the permission effect (it unconditionally overrides a grant).
func TestMigrateV72toV73_CollapsesGenerationsDeterministically(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	groupID, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err, "CreateAgentGroup")

	seedV72ConvKeyedIdentity(t, d)

	// One actor, two generations: old → new (succession edge ⇒ same actor).
	enroll(t, d, "old", "spawn", "", "")
	enroll(t, d, "new", "reincarnate", "", "")
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old', 'new', 'reincarnate', '2020-01-02T00:00:00Z')`)

	// Both generations are members of alpha; the newer one (new) holds 'lead'.
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (?, 'old', 'member', '', '2020-01-01T00:00:00Z')`, groupID)
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (?, 'new', 'lead', '', '2020-01-02T00:00:00Z')`, groupID)

	// Conflicting permission overrides for the same slug: the OLDER generation
	// denies, the NEWER grants. DENY must win despite being older.
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('old', 'self.compact', '2020-01-01T00:00:00Z', '', 'deny')`)
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('new', 'self.compact', '2020-01-02T00:00:00Z', '', 'grant')`)

	require.NoError(t, migrateV72toV73(d), "v72→v73 cutover")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 73, ver)

	// old + new collapse to one actor.
	oldA, err := AgentIDForConv("old")
	require.NoError(t, err)
	newA, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, oldA)
	assert.Equal(t, oldA, newA, "the reincarnation chain is one actor")
	assert.Equal(t, 1, countAgents(t, d), "one actor for the whole chain")

	// Exactly one membership row, agent-keyed, newest role ('lead') survived.
	var memCount int
	var memAgent, memRole string
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*), MAX(agent_id), MAX(role) FROM agent_group_members WHERE group_id = ?`,
		groupID).Scan(&memCount, &memAgent, &memRole))
	assert.Equal(t, 1, memCount, "two generations' memberships collapsed to one")
	assert.Equal(t, oldA, memAgent, "membership is keyed on the actor")
	assert.Equal(t, "lead", memRole, "newest generation's role wins the collapse")

	// Exactly one permission row, and DENY won the grant/deny collision.
	var permCount int
	var permEffect string
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*), MAX(effect) FROM agent_permissions WHERE agent_id = ? AND slug = 'self.compact'`,
		oldA).Scan(&permCount, &permEffect))
	assert.Equal(t, 1, permCount, "two generations' overrides collapsed to one")
	assert.Equal(t, "deny", permEffect, "DENY wins a grant/deny collapse, regardless of recency")
}

// TestUnmappedIdentityRows_DetectsOrphan checks the strict coverage gate: it
// counts identity rows whose conv has no agent_conversations mapping (the rows
// the destructive rebuild would silently drop). A mapped conv is fine; an
// unmapped one is reported so migrateV72toV73 can abort instead of losing it.
func TestUnmappedIdentityRows_DetectsOrphan(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	seedV72ConvKeyedIdentity(t, d)

	// One actor mapping 'mapped'; 'orphan' is deliberately left unmapped.
	agentID := newAgentID()
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at, created_via)
		VALUES (?, 'mapped', '2020-01-01T00:00:00Z', 'test')`, agentID)
	mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, role, reason, linked_at)
		VALUES ('mapped', ?, 'head', 'test', '2020-01-01T00:00:00Z')`, agentID)

	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (1, 'mapped', 'member', '', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (1, 'orphan', 'member', '', '2020-01-01T00:00:00Z')`)

	unmapped, err := unmappedIdentityRows(d)
	require.NoError(t, err)
	assert.Equal(t, 1, unmapped["agent_group_members"], "only the orphan conv is unmapped")
	assert.NotContains(t, unmapped, "agent_permissions", "no permission rows ⇒ not reported")
}

// seedV72ConvKeyedIdentity tears down the head (v73, agent-keyed) identity layer
// and rebuilds it in the v72 conv-keyed shape, then pins the version to 72 — so
// a test can drive the real v72→v73 cutover (or exercise its coverage gate) over
// hand-seeded conv-keyed rows. Only the columns the cutover reads are modelled.
func seedV72ConvKeyedIdentity(t *testing.T, d *sql.DB) {
	t.Helper()
	resetAgentLayer(t, d) // clears agents / agent_conversations / agent_enrollment
	for _, tbl := range []string{
		"agent_group_members", "agent_group_owners",
		"agent_permissions", "agent_sudo_grants", "agent_notify_prefs",
	} {
		mustExec(t, d, `DROP TABLE IF EXISTS `+tbl)
	}
	mustExec(t, d, `CREATE TABLE agent_group_members (
		group_id INTEGER NOT NULL, conv_id TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT '', descr TEXT NOT NULL DEFAULT '',
		joined_at TEXT NOT NULL, PRIMARY KEY (group_id, conv_id))`)
	mustExec(t, d, `CREATE TABLE agent_group_owners (
		group_id INTEGER NOT NULL, conv_id TEXT NOT NULL,
		granted_at TEXT NOT NULL, granted_by TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (group_id, conv_id))`)
	mustExec(t, d, `CREATE TABLE agent_permissions (
		conv_id TEXT NOT NULL, slug TEXT NOT NULL, granted_at TEXT NOT NULL,
		granted_by TEXT NOT NULL DEFAULT '',
		effect TEXT NOT NULL DEFAULT 'grant' CHECK (effect IN ('grant', 'deny')),
		PRIMARY KEY (conv_id, slug))`)
	mustExec(t, d, `CREATE TABLE agent_sudo_grants (
		id INTEGER PRIMARY KEY AUTOINCREMENT, conv_id TEXT NOT NULL, slug TEXT NOT NULL,
		granted_at TEXT NOT NULL, expires_at TEXT NOT NULL, granted_by TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '', revoked_at TEXT NOT NULL DEFAULT '')`)
	mustExec(t, d, `CREATE TABLE agent_notify_prefs (
		conv_id TEXT PRIMARY KEY, mode TEXT NOT NULL, updated_at TEXT NOT NULL)`)
	mustExec(t, d, `UPDATE schema_version SET version = 72`)
}
