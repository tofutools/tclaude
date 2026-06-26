package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcileAgentRoster_HealsMissingActor pins the JOH-26 PR3b self-heal: an
// active enrollment whose best-effort actor dual-write never landed (no agents
// row) is invisible to the actor-level roster until reconciled. The startup
// reconcile mints the missing actor so it returns to the active roster.
func TestReconcileAgentRoster_HealsMissingActor(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	// An active enrollment with NO actor row — the drift a failed dual-write
	// leaves on a head DB. Raw INSERT so the EnrollAgent dual-write doesn't fire.
	mustExec(t, d, `INSERT INTO agent_enrollment (conv_id, enrolled_at, enrolled_via)
		VALUES ('orphan-conv', '2020-01-01T00:00:00Z', 'spawn')`)
	require.Empty(t, mustAgentID(t, "orphan-conv"), "precondition: no actor yet")

	require.NoError(t, ReconcileAgentRoster(), "reconcile")

	agentID := mustAgentID(t, "orphan-conv")
	require.NotEmpty(t, agentID, "reconcile mints the missing actor")
	active, err := ListActiveAgents()
	require.NoError(t, err)
	assert.True(t, hasAgent(active, "orphan-conv"),
		"the healed actor is on the active roster at its current conv")
}

// TestReconcileAgentRoster_SyncsStuckRetiredFlag pins the other drift direction:
// a half-applied retire (enrollment retired, actor still active) leaves the
// agent stuck on the ACTIVE roster — and a re-retire is a no-op. The reconcile
// mirrors the current generation's enrollment retire state onto the actor.
func TestReconcileAgentRoster_SyncsStuckRetiredFlag(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	// Enroll + mint the actor cleanly (dual-write lands), then forge the drift:
	// retire ONLY the enrollment, leaving the actor active.
	require.NoError(t, EnrollAgent("stuck-conv", "spawn"))
	mustExec(t, d, `UPDATE agent_enrollment
		SET retired_at = '2020-02-02T00:00:00Z', retired_by = 'human', retire_reason = 'done'
		WHERE conv_id = 'stuck-conv'`)
	agentID := mustAgentID(t, "stuck-conv")
	require.NotEmpty(t, agentID)
	a, _ := GetAgent(agentID)
	require.True(t, a.Active(), "precondition: actor still active (the drift)")

	require.NoError(t, ReconcileAgentRoster(), "reconcile")

	a, _ = GetAgent(agentID)
	require.NotNil(t, a)
	assert.False(t, a.Active(), "reconcile retires the actor from its current enrollment")
	assert.Equal(t, "human", a.RetiredBy, "audit fields are carried across too")
	retired, err := ListRetiredAgents()
	require.NoError(t, err)
	assert.True(t, hasAgent(retired, "stuck-conv"), "the synced actor is now on the retired roster")
	active, err := ListActiveAgents()
	require.NoError(t, err)
	assert.False(t, hasAgent(active, "stuck-conv"), "and no longer on the active roster")
}

// TestReconcileAgentRoster_LeavesReincarnatePredecessorAlone guards the key
// invariant: the retired-flag sync keys on the actor's CURRENT conv only, so a
// reincarnate predecessor (its OLD enrollment retired, the actor active under
// the NEW conv) must stay active — the predecessor's retired enrollment must NOT
// drag the live actor onto the retired roster.
func TestReconcileAgentRoster_LeavesReincarnatePredecessorAlone(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	resetAgentLayer(t, d)

	require.NoError(t, EnrollAgent("old-gen", "spawn"))
	_, err = MigrateAgentIdentity("old-gen", "new-gen", "reincarnate", "system:test")
	require.NoError(t, err, "reincarnate")
	agentID := mustAgentID(t, "new-gen")
	require.NotEmpty(t, agentID)
	require.Equal(t, agentID, mustAgentID(t, "old-gen"), "both generations are one actor")

	require.NoError(t, ReconcileAgentRoster(), "reconcile")

	a, _ := GetAgent(agentID)
	require.NotNil(t, a)
	assert.True(t, a.Active(),
		"the live actor stays active despite its predecessor's retired enrollment")
	assert.Equal(t, "new-gen", a.CurrentConvID, "current conv unchanged")
}

func mustAgentID(t *testing.T, conv string) string {
	t.Helper()
	id, err := AgentIDForConv(conv)
	require.NoError(t, err, "AgentIDForConv(%s)", conv)
	return id
}

func hasAgent(agents []*Agent, conv string) bool {
	for _, a := range agents {
		if a.CurrentConvID == conv {
			return true
		}
	}
	return false
}
