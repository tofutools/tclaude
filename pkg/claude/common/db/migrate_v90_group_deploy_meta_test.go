package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV89toV90_AddsColumns drives the real v89→v90 ALTER over a
// v89-pinned DB: it asserts the two deployment-provenance columns appear, that
// a pre-existing group reads them back as '' (no provenance), that the version
// advances, and that a re-run is a clean no-op.
func TestMigrateV89toV90_AddsColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v89 and drop the new columns so we re-add them from a true v89
	// shape (the fresh chain already ran v90). SQLite supports DROP COLUMN.
	for _, col := range []string{"mission", "source_template"} {
		mustExec(t, d, `ALTER TABLE agent_groups DROP COLUMN `+col)
	}
	mustExec(t, d, `UPDATE schema_version SET version = 89`)

	// A pre-existing group (without the new columns) must survive the ALTER and
	// read the provenance fields back as the defaults.
	mustExec(t, d, `INSERT INTO agent_groups (name, descr, created_at)
		VALUES ('legacy', 'd', '2026-07-03T00:00:00Z')`)

	require.NoError(t, migrateV89toV90(d), "v89→v90")

	for _, col := range []string{"mission", "source_template"} {
		var n int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = ?`, col).Scan(&n))
		assert.Equalf(t, 1, n, "agent_groups.%s added", col)
	}

	// The legacy group reads its provenance back through the DB layer as unset.
	g, err := GetAgentGroupByName("legacy")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.Mission, "legacy group has no mission")
	assert.Empty(t, g.SourceTemplate, "legacy group has no source template")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 90, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op.
	require.NoError(t, migrateV89toV90(d), "v89→v90 re-run is a clean no-op")
}

// TestSetAgentGroupDeployMeta_RoundTrip proves the setter persists and the
// scan reads back the deployment provenance.
func TestSetAgentGroupDeployMeta_RoundTrip(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	_, err = CreateAgentGroup("force", "d")
	require.NoError(t, err)

	n, err := SetAgentGroupDeployMeta("force", "Ship the OAuth flow", "feature-team")
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "one group updated")

	g, err := GetAgentGroupByName("force")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "Ship the OAuth flow", g.Mission)
	assert.Equal(t, "feature-team", g.SourceTemplate)

	// A no-such-group update reports zero rows affected.
	n, err = SetAgentGroupDeployMeta("ghost", "x", "y")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}
