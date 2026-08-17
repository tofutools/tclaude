package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The default: nothing selected, so the floor applies. This is the assertion
// that would catch the axis silently reverting to the pre-floor posture.
func TestSpawnHarnessConfigDefaultsToTheFloor(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessDefault, snapshot.Effective.HarnessConfig)
	assert.True(t, sandboxpolicy.HarnessConfigFloorApplies(snapshot.Effective.HarnessConfig))
}

func TestSpawnHarnessConfigHumanMaySelectEitherPosture(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	for _, tc := range []struct {
		name  string
		wire  string
		want  sandboxpolicy.HarnessConfigAccess
		floor bool
	}{
		{name: "writable", wire: "write", want: sandboxpolicy.HarnessConfigAccessWrite},
		{name: "pinned", wire: "read", want: sandboxpolicy.HarnessConfigAccessRead, floor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawn := f.AsHuman().SpawnWith("crew", map[string]any{
				"name": "worker-" + tc.name, "harness_config": tc.wire,
			})
			require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
			snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
			require.True(t, ok)
			require.NotNil(t, snapshot)
			assert.Equal(t, tc.want, snapshot.Effective.HarnessConfig)
			assert.Equal(t, tc.floor,
				sandboxpolicy.HarnessConfigFloorApplies(snapshot.Effective.HarnessConfig))
		})
	}
}

func TestSpawnHarnessConfigRejectsUnknownPosture(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	bad := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness_config": "deny",
	})
	require.Equal(t, http.StatusBadRequest, bad.Code)
	assert.Contains(t, string(bad.Raw), "invalid_harness_config")
}

// The slug gate: an agent that can spawn still cannot choose the posture, and
// group ownership does not confer it. Granting the slug admits the selection.
func TestSpawnHarnessConfigNeedsTheSlugForAgentCallers(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "lead", "harness_config": "write",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))

	denied := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "harness_config": "write",
	})
	require.Equal(t, http.StatusForbidden, denied.Code)
	assert.Contains(t, string(denied.Raw), agentd.PermSandboxHarnessConfig)

	require.NoError(t, db.GrantAgentPermission(
		parent.ConvID, agentd.PermSandboxHarnessConfig, "test"))
	allowed := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child-ok", "harness_config": "write",
	})
	require.Equalf(t, http.StatusOK, allowed.Code, "spawn body=%s", allowed.Raw)
	snapshot, ok := f.World.SpawnSandboxPolicy(allowed.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, snapshot.Effective.HarnessConfig)
}

// The slug gates SELECTION, not widening. A floored parent holding the slug
// still cannot hand an unfloored posture to a child — lineage decides that.
func TestSpawnHarnessConfigSlugCannotWidenPastTheParent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	parent := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "lead", "harness_config": "read",
	})
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(
		parent.ConvID, agentd.PermSandboxHarnessConfig, "test"))

	widened := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "harness_config": "write",
	})
	require.NotEqualf(t, http.StatusOK, widened.Code,
		"a floored parent must not mint an unfloored child, body=%s", widened.Raw)
}

// omit_sandbox_profiles records "no profile tier applied at all"; writing a
// posture into that snapshot would turn a fail-closed marker into an error.
func TestSpawnHarnessConfigRefusedAlongsideOmittedProfiles(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	bad := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "omit_sandbox_profiles": true, "harness_config": "write",
	})
	require.Equal(t, http.StatusUnprocessableEntity, bad.Code)
	assert.Contains(t, string(bad.Raw), "invalid_harness_config")

	// Pinning the floor is compatible with omitted profiles: it is what an
	// absent value already means, so nothing is written and nothing refuses.
	ok := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker-pinned", "omit_sandbox_profiles": true, "harness_config": "read",
	})
	require.Equalf(t, http.StatusOK, ok.Code, "spawn body=%s", ok.Raw)
}
