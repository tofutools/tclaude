package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV98toV99_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. The literal currentVersion
// tripwire lives on the head migration's test.
func TestMigrateV98toV99_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
}

// TestMigrateV98toV99_AddsColumn drives the real v98→v99 ALTER over a v98-pinned
// DB: it asserts agent_groups.parent_id appears, that a pre-existing group reads
// back NULL (top-level), that the version advances, and that a re-run is a clean
// no-op.
func TestMigrateV98toV99_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v98 and drop the new column so we re-add it from a true v98
	// shape (the fresh chain already ran v99). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE agent_groups DROP COLUMN parent_id`)
	mustExec(t, d, `UPDATE schema_version SET version = 98`)

	// A pre-existing group row (without the new column) must survive the ALTER.
	mustExec(t, d, `INSERT INTO agent_groups (name, created_at) VALUES ('legacy-grp', '2026-07-04T00:00:00Z')`)

	require.NoError(t, migrateV98toV99(d), "v98→v99")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = 'parent_id'`).Scan(&n))
	assert.Equal(t, 1, n, "agent_groups.parent_id added")

	// The pre-existing group reads back with a NULL parent (top-level).
	var parent any
	require.NoError(t, d.QueryRow(
		`SELECT parent_id FROM agent_groups WHERE name = 'legacy-grp'`).Scan(&parent))
	assert.Nil(t, parent, "existing group defaults to top-level (NULL parent)")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 99, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV98toV99(d), "v98→v99 re-run is a clean no-op")
}

// TestSetAgentGroupParent covers the nesting mutation end-to-end against a fully
// migrated DB: set + clear a parent, the two cycle guards (self and descendant),
// missing-parent, and — the crux — that deleting a parent auto-orphans its
// children back to top-level via the column's ON DELETE SET NULL foreign key.
func TestSetAgentGroupParent(t *testing.T) {
	setupTestDB(t)

	aID, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err)
	bID, err := CreateAgentGroup("beta", "")
	require.NoError(t, err)
	_, err = CreateAgentGroup("gamma", "")
	require.NoError(t, err)

	// Nest beta under alpha.
	g, err := SetAgentGroupParent(bID, "alpha")
	require.NoError(t, err)
	require.NotNil(t, g.ParentGroupID)
	assert.Equal(t, aID, *g.ParentGroupID, "beta nested under alpha")

	// Self-parent is refused.
	_, err = SetAgentGroupParent(aID, "alpha")
	assert.ErrorIs(t, err, ErrGroupParentCycle, "self-parent refused")

	// Descendant cycle refused: alpha under beta (beta is already alpha's child).
	_, err = SetAgentGroupParent(aID, "beta")
	assert.ErrorIs(t, err, ErrGroupParentCycle, "descendant cycle refused")

	// Missing parent is a not-found.
	_, err = SetAgentGroupParent(bID, "no-such-group")
	assert.ErrorIs(t, err, ErrGroupParentNotFound, "missing parent refused")

	// Clear the parent → top-level.
	g, err = SetAgentGroupParent(bID, "")
	require.NoError(t, err)
	assert.Nil(t, g.ParentGroupID, "parent cleared → top-level")

	// The robustness requirement: deleting a parent auto-orphans children.
	require.NoError(t, err)
	_, err = SetAgentGroupParent(bID, "alpha")
	require.NoError(t, err)
	require.NoError(t, DeleteAgentGroup("alpha"))
	after, err := GetAgentGroupByID(bID)
	require.NoError(t, err)
	require.NotNil(t, after, "beta still exists after its parent is deleted")
	assert.Nil(t, after.ParentGroupID, "deleting the parent auto-orphaned beta to top-level (ON DELETE SET NULL)")
}
