package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV74toV75_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts agent_enrollment is gone at head. v75 is head, so the
// literal currentVersion tripwire lives here now (moved forward from the v74
// test); the next migration's author moves it into their own test.
func TestMigrateV74toV75_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 75, currentVersion, "tripwire: bump this and add a v75→v76 test when you add a migration")

	exists, err := tableExists(d, "agent_enrollment")
	require.NoError(t, err)
	assert.False(t, exists, "agent_enrollment is dropped at head (JOH-26 PR3c)")
}

// TestMigrateV74toV75_DropsEnrollment stages a DB that still carries
// agent_enrollment (an older DB mid-chain) and drives the real v74→v75 drop. The
// table — and its partial index — must be gone afterward, while the actor layer
// (the sole roster since PR3c) is untouched.
func TestMigrateV74toV75_DropsEnrollment(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Re-stand-up the v30-era table (head already dropped it), seed a row + the
	// active partial index, plus an actor that must survive the drop.
	ensureEnrollmentTableForTest(t, d)
	mustExec(t, d, `CREATE INDEX IF NOT EXISTS idx_agent_enrollment_active
		ON agent_enrollment(conv_id) WHERE retired_at = ''`)
	mustExec(t, d, `INSERT INTO agent_enrollment (conv_id, enrolled_at, enrolled_via)
		VALUES ('c1', '2020-01-01T00:00:00Z', 'spawn')`)
	_, _, err = EnsureAgentForConv("c1", "spawn")
	require.NoError(t, err)

	require.NoError(t, migrateV74toV75(d), "migrateV74toV75")

	exists, err := tableExists(d, "agent_enrollment")
	require.NoError(t, err)
	assert.False(t, exists, "agent_enrollment dropped")

	// The drop is idempotent — a re-run on the now-absent table is a clean no-op.
	require.NoError(t, migrateV74toV75(d), "migrateV74toV75 re-run")

	// The actor layer is the sole roster and survives the drop.
	agentID, err := AgentIDForConv("c1")
	require.NoError(t, err)
	assert.NotEmpty(t, agentID, "the actor survives the enrollment drop")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 75, ver, "schema version advanced to 75")
}
