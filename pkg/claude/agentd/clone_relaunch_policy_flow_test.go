package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tcl-bepvre: a clone in its source's groups is a relaunch of the source as
// a new agent, so the relaunch policy's group_env and group_remote_control
// rows apply to it as they would to the source's own resume.

func TestCloneRelaunchPolicy_GroupEnv(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	setGroupEnv(t, "v1")
	conv, _ := spawnPinned(t, f, "env-source", "")

	setRelaunchPolicy(t, f, "group_env", "reuse")
	setGroupEnv(t, "v2")
	clone := f.AsHuman().CloneFresh(conv)
	policy, ok := f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v1", launchEnvValue(t, policy, "CREW_ENV"), "reuse: the clone keeps the env its source last launched with")

	setRelaunchPolicy(t, f, "group_env", "re-resolve")
	clone = f.AsHuman().CloneFresh(conv)
	policy, ok = f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v2", launchEnvValue(t, policy, "CREW_ENV"), "re-resolve (default): the group's current env")
}

func TestCloneRelaunchPolicy_GroupRemoteControl(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	conv, _ := spawnPinned(t, f, "rc-source", "")
	on := true
	setGroupRemoteControl(t, &on)

	setRelaunchPolicy(t, f, "group_remote_control", "reuse")
	clone := f.AsHuman().CloneFresh(conv)
	got, ok := f.World.SpawnRemoteControl(clone.NewConv)
	require.True(t, ok)
	assert.False(t, got, "reuse: the clone keeps its source's last state")

	setRelaunchPolicy(t, f, "group_remote_control", "re-resolve")
	clone = f.AsHuman().CloneFresh(conv)
	got, ok = f.World.SpawnRemoteControl(clone.NewConv)
	require.True(t, ok)
	assert.True(t, got, "re-resolve: the group's current policy")
}
