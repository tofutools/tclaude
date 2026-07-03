package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGroupTemplateProcess_RoundTrip proves the process spec persists on a
// template and reads back intact.
func TestGroupTemplateProcess_RoundTrip(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	phases := []ProcessPhase{
		{Name: "design", Roles: []string{"architect"}, Criteria: "a plan exists"},
		{Name: "build", Roles: []string{"dev", "all"}, Criteria: "code compiles"},
		{Name: "review", Roles: []string{"reviewer"}},
	}
	id, err := CreateGroupTemplate(&GroupTemplate{Name: "proc-tmpl", Process: phases})
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := GetGroupTemplate("proc-tmpl")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, phases, got.Process)

	// Update replaces the process wholesale.
	got.Process = []ProcessPhase{{Name: "solo", Roles: []string{}}}
	require.NoError(t, UpdateGroupTemplate(got))
	after, err := GetGroupTemplate("proc-tmpl")
	require.NoError(t, err)
	require.Len(t, after.Process, 1)
	assert.Equal(t, "solo", after.Process[0].Name)
	assert.NotNil(t, after.Process[0].Roles, "empty roles reads back non-nil")
}

// TestGroupProcessState_Lifecycle drives init → get → advance → transitions,
// plus the no-process no-op and the group-scoped cleanup.
func TestGroupProcessState_Lifecycle(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	gid, err := CreateAgentGroup("proc-grp", "d")
	require.NoError(t, err)

	// No process → InitGroupProcess is a no-op and no state exists.
	require.NoError(t, InitGroupProcess(gid, nil, "human"))
	st, err := GetGroupProcessState(gid)
	require.NoError(t, err)
	assert.Nil(t, st, "no process → no state")

	phases := []ProcessPhase{
		{Name: "design", Roles: []string{"architect"}, Criteria: "planned"},
		{Name: "build", Roles: []string{"dev"}, Criteria: "coded"},
	}
	require.NoError(t, InitGroupProcess(gid, phases, "human"))

	st, err = GetGroupProcessState(gid)
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.Equal(t, "design", st.CurrentPhase, "starts at first phase")
	assert.Equal(t, 0, st.PhaseIndex())
	assert.Equal(t, phases, st.Process)
	assert.False(t, st.PhaseStartedAt.IsZero())

	// One initial transition from "" → first phase.
	trs, err := ListGroupProcessTransitions(gid)
	require.NoError(t, err)
	require.Len(t, trs, 1)
	assert.Equal(t, "", trs[0].FromPhase)
	assert.Equal(t, "design", trs[0].ToPhase)
	assert.Equal(t, "human", trs[0].Actor)

	// Advance to the next phase records a second transition.
	require.NoError(t, AdvanceGroupProcess(gid, "build", "agt_lead"))
	st, err = GetGroupProcessState(gid)
	require.NoError(t, err)
	assert.Equal(t, "build", st.CurrentPhase)
	assert.Equal(t, 1, st.PhaseIndex())
	trs, err = ListGroupProcessTransitions(gid)
	require.NoError(t, err)
	require.Len(t, trs, 2)
	assert.Equal(t, "design", trs[1].FromPhase)
	assert.Equal(t, "build", trs[1].ToPhase)
	assert.Equal(t, "agt_lead", trs[1].Actor)

	// Deleting the group sweeps the process state + transitions.
	require.NoError(t, DeleteAgentGroup("proc-grp"))
	st, err = GetGroupProcessState(gid)
	require.NoError(t, err)
	assert.Nil(t, st, "state swept on group delete")
	trs, err = ListGroupProcessTransitions(gid)
	require.NoError(t, err)
	assert.Empty(t, trs, "transitions swept on group delete")
}
