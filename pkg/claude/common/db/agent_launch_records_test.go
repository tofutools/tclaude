package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// A re-pin reads the stored snapshot, then writes a replacement: it must never
// overwrite a snapshot a relaunch recorded in between.
func TestReplaceAgentEffectiveSandboxConfigRefusesAChangedRecord(t *testing.T) {
	setupTestDB(t)
	agentID, _, err := EnsureAgentForConv("launch-record-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, SetAgentInitialSpawnConfig(agentID, `{"harness_config":"read"}`))
	first := sandboxpolicy.EmptySnapshot()
	require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &first))

	records, err := ListActiveAgentLaunchRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	read := records[0]
	assert.Equal(t, `{"harness_config":"read"}`, read.InitialSpawnConfig)

	relaunched := sandboxpolicy.EmptySnapshot()
	relaunched.Effective.HarnessConfig = sandboxpolicy.HarnessConfigAccessWrite
	require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &relaunched))

	repinned := sandboxpolicy.EmptySnapshot()
	repinned.Effective.HarnessConfig = sandboxpolicy.HarnessConfigAccessRead
	require.ErrorIs(t, ReplaceAgentEffectiveSandboxConfig(agentID, read.EffectiveSandboxRaw, &repinned),
		ErrAgentLaunchRecordChanged)
	stored, err := AgentEffectiveSandboxConfigForConv("launch-record-conv")
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, stored.Effective.HarnessConfig, "the relaunch's record stands")

	records, err = ListActiveAgentLaunchRecords()
	require.NoError(t, err)
	require.NoError(t, ReplaceAgentEffectiveSandboxConfig(agentID, records[0].EffectiveSandboxRaw, &repinned))
	stored, err = AgentEffectiveSandboxConfigForConv("launch-record-conv")
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessRead, stored.Effective.HarnessConfig)
}
