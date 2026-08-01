package db

import (
	"database/sql"
	"testing"
	"time"

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
	normalGroupID, err := CreateAgentGroup("beta", "")
	require.NoError(t, err, "CreateAgentGroup beta")

	seedV72ConvKeyedIdentity(t, d)

	// One actor, two generations: old → new (succession edge ⇒ same actor).
	enroll(t, d, "old", "spawn", "", "")
	enroll(t, d, "new", "reincarnate", "", "")
	actorID := newAgentID()
	createdAt := dbTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at, created_via)
		VALUES (?, 'new', ?, 'test')`, actorID, createdAt)
	mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, role, reason, linked_at)
		VALUES ('old', ?, 'generation', 'test', ?), ('new', ?, 'head', 'test', ?)`,
		actorID, createdAt, actorID, createdAt)
	mustExec(t, d, `INSERT INTO agent_conv_succession (old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES ('old', 'new', 'reincarnate', ?)`, dbTime(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)))

	// A second actor supplies the non-inverting notify control; notify prefs
	// have only one row per generation, unlike memberships/permissions where a
	// second group or slug can carry both controls for the same actor.
	enroll(t, d, "control-old", "spawn", "", "")
	enroll(t, d, "control-new", "reincarnate", "", "")
	controlActorID := newAgentID()
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at, created_via)
		VALUES (?, 'control-new', ?, 'test')`, controlActorID, createdAt)
	mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, role, reason, linked_at)
		VALUES ('control-old', ?, 'generation', 'test', ?), ('control-new', ?, 'head', 'test', ?)`,
		controlActorID, createdAt, controlActorID, createdAt)

	// Both generations are members of alpha; the newer one (new) holds 'lead'.
	// These spellings are lexically inverted across offsets: old is 00:59Z and
	// new is 01:00Z even though old's wall-clock text (02:59) sorts later.
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (?, 'old', 'member', '', '2020-01-01T02:59:00+02:00')`, groupID)
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (?, 'new', 'lead', '', '2020-01-01T02:00:00+01:00')`, groupID)
	// Same table, negative control: ordinary UTC spellings already choose new.
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (?, 'old', 'member', '', '2020-01-01T00:00:00Z')`, normalGroupID)
	mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (?, 'new', 'lead', '', '2020-01-02T00:00:00Z')`, normalGroupID)

	// A trimmed whole-second spelling sorts after its later fractional
	// extension as text. Exact canonicalization must still keep the latter.
	mustExec(t, d, `INSERT INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
		VALUES (?, 'old', '2020-01-01T00:00:07Z', 'old')`, groupID)
	mustExec(t, d, `INSERT INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
		VALUES (?, 'new', '2020-01-01T00:00:07.000001Z', 'new')`, groupID)
	mustExec(t, d, `INSERT INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
		VALUES (?, 'old', '2020-01-01T00:00:00Z', 'old')`, normalGroupID)
	mustExec(t, d, `INSERT INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
		VALUES (?, 'new', '2020-01-02T00:00:00Z', 'new')`, normalGroupID)

	// Notify has its own inverting pair; the second actor supplies the ordinary
	// same-zone negative control.
	mustExec(t, d, `INSERT INTO agent_notify_prefs (conv_id, mode, updated_at)
		VALUES ('old', 'off', '2020-01-01T02:59:00+02:00')`)
	mustExec(t, d, `INSERT INTO agent_notify_prefs (conv_id, mode, updated_at)
		VALUES ('new', 'on', '2020-01-01T02:00:00+01:00')`)
	mustExec(t, d, `INSERT INTO agent_notify_prefs (conv_id, mode, updated_at)
		VALUES ('control-old', 'off', '2020-01-01T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agent_notify_prefs (conv_id, mode, updated_at)
		VALUES ('control-new', 'on', '2020-01-02T00:00:00Z')`)

	// Conflicting permission overrides for the same slug: the OLDER generation
	// denies, the NEWER grants. DENY must win despite being older.
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('old', 'self.compact', '2020-01-01T00:00:00Z', '', 'deny')`)
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('new', 'self.compact', '2020-01-02T00:00:00Z', '', 'grant')`)
	// Equal-effect permissions fall through to instant ordering. One pair is
	// lexically inverted and one is an already-correct negative control.
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('old', 'test.equal-inverted', '2020-01-01T02:59:00+02:00', 'old', 'grant')`)
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('new', 'test.equal-inverted', '2020-01-01T02:00:00+01:00', 'new', 'grant')`)
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('old', 'test.equal-normal', '2020-01-01T00:00:00Z', 'old', 'grant')`)
	mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
		VALUES ('new', 'test.equal-normal', '2020-01-02T00:00:00Z', 'new', 'grant')`)

	// Sudo rows do not collapse, but all three timestamp columns still pass
	// through the same exact canonicalizer. Empty revoked_at is its sole
	// optional timestamp and must remain empty.
	mustExec(t, d, `INSERT INTO agent_sudo_grants
		(id, conv_id, slug, granted_at, expires_at, granted_by, revoked_at)
		VALUES (77, 'old', 'test.sudo', '2020-01-01T02:59:00+02:00',
			'2020-01-01T02:00:00+01:00', 'fixture', '')`)

	// Controls prove both ordering shapes before the migration runs: the old
	// lexical query really does choose the wrong member for the offset pair,
	// while it chooses the right notify preference for the non-inverting pair.
	var lexicalRole, normalRole, lexicalOwner, normalOwner, lexicalMode, normalMode, lexicalPermission, normalPermission string
	require.NoError(t, d.QueryRow(`SELECT role FROM agent_group_members WHERE group_id = ? ORDER BY joined_at DESC LIMIT 1`, groupID).
		Scan(&lexicalRole))
	assert.Equal(t, "member", lexicalRole, "control: legacy lexical ordering must reproduce the defect")
	require.NoError(t, d.QueryRow(`SELECT role FROM agent_group_members WHERE group_id = ? ORDER BY joined_at DESC LIMIT 1`, normalGroupID).Scan(&normalRole))
	assert.Equal(t, "lead", normalRole, "member negative control starts correct")
	require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_group_owners WHERE group_id = ? ORDER BY granted_at DESC LIMIT 1`, groupID).Scan(&lexicalOwner))
	assert.Equal(t, "old", lexicalOwner, "owner lexical control reproduces the defect")
	require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_group_owners WHERE group_id = ? ORDER BY granted_at DESC LIMIT 1`, normalGroupID).Scan(&normalOwner))
	assert.Equal(t, "new", normalOwner, "owner negative control starts correct")
	require.NoError(t, d.QueryRow(`SELECT mode FROM agent_notify_prefs WHERE conv_id IN ('old','new') ORDER BY updated_at DESC LIMIT 1`).Scan(&lexicalMode))
	assert.Equal(t, "off", lexicalMode, "notify lexical control reproduces the defect")
	require.NoError(t, d.QueryRow(`SELECT mode FROM agent_notify_prefs WHERE conv_id IN ('control-old','control-new') ORDER BY updated_at DESC LIMIT 1`).Scan(&normalMode))
	assert.Equal(t, "on", normalMode, "notify negative control starts correct")
	require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_permissions WHERE slug = 'test.equal-inverted' ORDER BY granted_at DESC LIMIT 1`).Scan(&lexicalPermission))
	assert.Equal(t, "old", lexicalPermission, "equal-effect permission lexical control reproduces the defect")
	require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_permissions WHERE slug = 'test.equal-normal' ORDER BY granted_at DESC LIMIT 1`).Scan(&normalPermission))
	assert.Equal(t, "new", normalPermission, "equal-effect permission negative control starts correct")

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
	assert.Equal(t, 2, countAgents(t, d), "two fixture actors, each stable across its chain")

	// Exactly one membership row, agent-keyed, newest role ('lead') survived.
	var memCount int
	var memAgent, memRole string
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*), MAX(agent_id), MAX(role) FROM agent_group_members WHERE group_id = ?`,
		groupID).Scan(&memCount, &memAgent, &memRole))
	assert.Equal(t, 1, memCount, "two generations' memberships collapsed to one")
	assert.Equal(t, oldA, memAgent, "membership is keyed on the actor")
	assert.Equal(t, "lead", memRole, "newest generation's role wins the collapse")
	var normalMemRole string
	require.NoError(t, d.QueryRow(`SELECT role FROM agent_group_members WHERE group_id = ?`, normalGroupID).Scan(&normalMemRole))
	assert.Equal(t, "lead", normalMemRole, "member negative control survives")
	var ownerBy string
	require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_group_owners WHERE group_id = ?`, groupID).Scan(&ownerBy))
	assert.Equal(t, "new", ownerBy, "fractionally newer owner survives the collapse")
	var normalOwnerBy string
	require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_group_owners WHERE group_id = ?`, normalGroupID).Scan(&normalOwnerBy))
	assert.Equal(t, "new", normalOwnerBy, "owner negative control survives")
	var notifyMode, notifyStamp, controlNotifyMode string
	require.NoError(t, d.QueryRow(`SELECT mode, updated_at FROM agent_notify_prefs WHERE agent_id = ?`, oldA).Scan(&notifyMode, &notifyStamp))
	assert.Equal(t, "on", notifyMode, "canonical instant ordering repairs the inverting preference pair")
	assert.Equal(t, "2020-01-01T01:00:00.000000000Z", notifyStamp, "notify timestamp is positively canonicalized")
	require.NoError(t, d.QueryRow(`SELECT mode FROM agent_notify_prefs WHERE agent_id = ?`, controlActorID).Scan(&controlNotifyMode))
	assert.Equal(t, "on", controlNotifyMode, "notify negative control survives")

	// Exactly one permission row, and DENY won the grant/deny collision.
	var permCount int
	var permEffect string
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*), MAX(effect) FROM agent_permissions WHERE agent_id = ? AND slug = 'self.compact'`,
		oldA).Scan(&permCount, &permEffect))
	assert.Equal(t, 1, permCount, "two generations' overrides collapsed to one")
	assert.Equal(t, "deny", permEffect, "DENY wins a grant/deny collapse, regardless of recency")
	for _, slug := range []string{"test.equal-inverted", "test.equal-normal"} {
		var grantedBy string
		require.NoError(t, d.QueryRow(`SELECT granted_by FROM agent_permissions WHERE agent_id = ? AND slug = ?`, oldA, slug).Scan(&grantedBy))
		assert.Equal(t, "new", grantedBy, "%s uses instant order among equal effects", slug)
	}
	var grantedAt, expiresAt, revokedAt string
	require.NoError(t, d.QueryRow(`SELECT granted_at, expires_at, revoked_at FROM agent_sudo_grants WHERE id = 77`).Scan(&grantedAt, &expiresAt, &revokedAt))
	assert.Equal(t, "2020-01-01T00:59:00.000000000Z", grantedAt)
	assert.Equal(t, "2020-01-01T01:00:00.000000000Z", expiresAt)
	assert.Empty(t, revokedAt, "optional absence survives canonicalization")
}

func TestMigrateV72toV73_RejectsInvalidTimestampsLoudly(t *testing.T) {
	targets := []struct {
		table, column string
		optional      bool
		insert        func(*testing.T, *sql.DB, string)
	}{
		{"agent_group_members", "joined_at", false, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
				VALUES (1, 'mapped', 'member', '', ?)`, value)
		}},
		{"agent_group_owners", "granted_at", false, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
				VALUES (1, 'mapped', ?, 'fixture')`, value)
		}},
		{"agent_permissions", "granted_at", false, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_permissions (conv_id, slug, granted_at, granted_by, effect)
				VALUES ('mapped', 'test.invalid', ?, 'fixture', 'grant')`, value)
		}},
		{"agent_sudo_grants", "granted_at", false, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_sudo_grants (conv_id, slug, granted_at, expires_at, granted_by, revoked_at)
				VALUES ('mapped', 'test.invalid', ?, '2020-01-02T00:00:00Z', 'fixture', '')`, value)
		}},
		{"agent_sudo_grants", "expires_at", false, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_sudo_grants (conv_id, slug, granted_at, expires_at, granted_by, revoked_at)
				VALUES ('mapped', 'test.invalid', '2020-01-01T00:00:00Z', ?, 'fixture', '')`, value)
		}},
		{"agent_sudo_grants", "revoked_at", true, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_sudo_grants (conv_id, slug, granted_at, expires_at, granted_by, revoked_at)
				VALUES ('mapped', 'test.invalid', '2020-01-01T00:00:00Z', '2020-01-02T00:00:00Z', 'fixture', ?)`, value)
		}},
		{"agent_notify_prefs", "updated_at", false, func(t *testing.T, d *sql.DB, value string) {
			mustExec(t, d, `INSERT INTO agent_notify_prefs (conv_id, mode, updated_at)
				VALUES ('mapped', 'on', ?)`, value)
		}},
	}
	invalids := []struct{ name, value string }{
		{"empty", ""},
		{"malformed", "not-a-time"},
		{"zero", "1970-01-01T00:00:00Z"},
		{"out_of_range", "9999-01-01T00:00:00Z"},
	}
	for _, target := range targets {
		for _, tc := range invalids {
			if target.optional && tc.value == "" {
				continue
			}
			t.Run(target.table+"_"+target.column+"_"+tc.name, func(t *testing.T) {
				setupTestDB(t)
				d, err := Open()
				require.NoError(t, err)
				seedV72ConvKeyedIdentity(t, d)
				agentID := newAgentID()
				mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at, created_via)
				VALUES (?, 'mapped', ?, 'test')`, agentID, dbTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
				mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, role, reason, linked_at)
				VALUES ('mapped', ?, 'head', 'test', ?)`, agentID, dbTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
				target.insert(t, d, tc.value)

				err = migrateV72toV73(d)
				require.Error(t, err, "failure arm must execute")
				assert.ErrorContains(t, err, target.table+"."+target.column)
				assert.ErrorContains(t, err, "rowid")
				assert.ErrorContains(t, err, `value "`+tc.value+`"`)
				var version int
				require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
				assert.Equal(t, 72, version, "failed canonicalization must not advance")
				var stored string
				require.NoError(t, d.QueryRow(`SELECT `+quoteIdentifier(target.column)+` FROM `+quoteIdentifier(target.table)+` LIMIT 1`).Scan(&stored))
				assert.Equal(t, tc.value, stored, "failed canonicalization rolls back source spelling")
			})
		}
	}
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
		VALUES (?, 'mapped', 1577836800000000000, 'test')`, agentID)
	mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, role, reason, linked_at)
		VALUES ('mapped', ?, 'head', 'test', 1577836800000000000)`, agentID)

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
