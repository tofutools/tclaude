package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/startupctx"
)

// Launch slice 7b (tcl-io7bcl): a clone is its source's launch as a new agent,
// so it gets the records a first spawn writes for later relaunches and
// compactions to read: the launch layers and the startup snapshot.

// A clone's compaction re-injects what its source's would: the source opted
// out of the group's standing context and was spawned with a profile's
// startup context, and the clone keeps both. Before, a clone had no startup
// snapshot, so it got the group context and lost the profile's.
func TestCloneLaunchRecord_ReinjectionKeepsTheSourcesStartupContext(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.SetAgentGroupDefaultContext("crew", "GROUP STANDING CONTEXT")
	require.NoError(t, err)
	rec := createProfile(t, f, map[string]any{"name": "briefed", "startup_context": "PROFILE STARTUP CONTEXT"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "profile": "briefed", "include_group_context": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	clone := f.AsHuman().CloneFresh(spawn.ConvID)
	for _, conv := range []string{spawn.ConvID, clone.NewConv} {
		agentID, err := db.AgentIDForConv(conv)
		require.NoError(t, err)
		got, err := startupctx.ComposeReinjection(agentID, startupctx.BoundaryCompact)
		require.NoError(t, err)
		assert.Contains(t, got.Body, "PROFILE STARTUP CONTEXT", "conv %s", conv)
		assert.NotContains(t, got.Body, "GROUP STANDING CONTEXT", "conv %s opted out of the group context", conv)
	}
}

// A clone's explicit --harness-config is its source's (its launch layers are
// copied), so a chain-wins launch that records the profiles' posture does not
// lose it for good: switching back to explicit-wins restores the pin on the
// clone's next resume, as it does for the source.
func TestCloneLaunchRecord_ExplicitHarnessConfigComesBackAfterChainWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "chain", HarnessConfig: sandboxpolicy.HarnessConfigAccessWrite})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("chain"))
	conv, _ := spawnPinned(t, f, "pinned", "read")

	setRelaunchPolicy(t, f, "harness_config", "chain-wins")
	clone := f.AsHuman().CloneFresh(conv)
	born, ok := f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	require.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, born.Effective.HarnessConfig, "chain-wins: the clone takes the profiles' posture")

	setRelaunchPolicy(t, f, "harness_config", "explicit-wins")
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessRead, resumedHarnessConfig(t, f, clone.NewConv),
		"explicit-wins: the clone's next resume restores its source's pin")
}
