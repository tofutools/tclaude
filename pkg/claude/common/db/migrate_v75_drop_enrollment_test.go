package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV74toV75_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts agent_enrollment is gone at head. The literal currentVersion
// tripwire moved forward to the v76 test (head), per convention.
func TestMigrateV74toV75_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

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

// TestMigrateV74toV75_HealsDriftBeforeDrop pins the final enrollment→actor heal:
// a head DB upgraded from PR3b can have enrollment/actor drift (the dual-write
// was best-effort and the startup reconcile is gone), so v75 must fold the
// authoritative enrollment facts into the agents table BEFORE dropping it.
func TestMigrateV74toV75_HealsDriftBeforeDrop(t *testing.T) {
	t.Run("active enrollment with missing actor survives as an active actor", func(t *testing.T) {
		setupTestDB(t)
		d, err := Open()
		require.NoError(t, err, "Open")
		ensureEnrollmentTableForTest(t, d)

		// An active enrollment that never got its actor row (a dropped dual-write).
		mustExec(t, d, `INSERT INTO agent_enrollment (conv_id, enrolled_at, enrolled_via)
			VALUES ('a-conv', '2020-01-01T00:00:00Z', 'spawn')`)
		require.Empty(t, mustAgentIDForConv(t, "a-conv"), "precondition: no actor yet")

		require.NoError(t, migrateV74toV75(d), "migrateV74toV75")

		a, err := GetAgentByConv("a-conv")
		require.NoError(t, err)
		require.NotNil(t, a, "backfill heals the missing actor")
		assert.True(t, a.Active(), "and it comes up active")
	})

	t.Run("retired current enrollment flips the active actor to retired", func(t *testing.T) {
		setupTestDB(t)
		d, err := Open()
		require.NoError(t, err, "Open")

		// An actor that is active, but whose CURRENT-generation enrollment was
		// retired without the matching actor flip landing.
		agentID, _, err := EnsureAgentForConv("r-conv", "spawn")
		require.NoError(t, err)
		ensureEnrollmentTableForTest(t, d)
		mustExec(t, d, `INSERT INTO agent_enrollment
			(conv_id, enrolled_at, enrolled_via, retired_at, retired_by, retire_reason)
			VALUES ('r-conv', '2020-01-01T00:00:00Z', 'spawn', '2020-02-02T00:00:00Z', 'human', 'cleanup')`)
		pre, err := GetAgent(agentID)
		require.NoError(t, err)
		require.NotNil(t, pre)
		require.True(t, pre.Active(), "precondition: actor still active (drift)")

		require.NoError(t, migrateV74toV75(d), "migrateV74toV75")

		a, err := GetAgent(agentID)
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.False(t, a.Active(), "the actor is synced to retired from its current enrollment")
		assert.Equal(t, "human", a.RetiredBy, "audit fields carried too")
	})

	t.Run("retired PREDECESSOR enrollment must NOT retire the live actor", func(t *testing.T) {
		setupTestDB(t)
		d, err := Open()
		require.NoError(t, err, "Open")

		// One actor, two generations: old → new (new is the live head).
		_, _, err = EnsureAgentForConv("old-gen", "spawn")
		require.NoError(t, err)
		_, err = RotateAgentConv("old-gen", "new-gen", "reincarnate")
		require.NoError(t, err)
		agentID := mustAgentIDForConv(t, "new-gen")

		// The predecessor's enrollment is retired (as a rotation used to leave
		// it), the current generation's is active.
		ensureEnrollmentTableForTest(t, d)
		mustExec(t, d, `INSERT INTO agent_enrollment
			(conv_id, enrolled_at, enrolled_via, retired_at, retired_by, retire_reason)
			VALUES ('old-gen', '2020-01-01T00:00:00Z', 'spawn', '2020-02-02T00:00:00Z', 'system:reincarnate', 'superseded')`)
		mustExec(t, d, `INSERT INTO agent_enrollment (conv_id, enrolled_at, enrolled_via)
			VALUES ('new-gen', '2020-03-03T00:00:00Z', 'reincarnate')`)

		require.NoError(t, migrateV74toV75(d), "migrateV74toV75")

		a, err := GetAgent(agentID)
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.True(t, a.Active(),
			"the sync keys on the CURRENT generation — a retired predecessor enrollment must not retire the live actor")
	})
}

// mustAgentIDForConv resolves a conv to its actor id, failing the test on a
// read error. Returns "" when the conv has no actor.
func mustAgentIDForConv(t *testing.T, conv string) string {
	t.Helper()
	id, err := AgentIDForConv(conv)
	require.NoError(t, err)
	return id
}
