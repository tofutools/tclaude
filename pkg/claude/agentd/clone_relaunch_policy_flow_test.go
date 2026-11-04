package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tcl-bepvre: a clone in its source's groups is a relaunch of the source as
// a new agent, so the relaunch policy's group_env, sandbox_profile_contents
// and group_remote_control rows apply to it as they would to the source's own
// resume.

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

// A clone that joins another group but keeps its source's environment keeps
// it as the source's resume would, under the group_env row.
func TestCloneRelaunchPolicy_KeptEnvironmentFollowsGroupEnv(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	f.HaveGroup("other")
	setGroupEnv(t, "v1")
	conv, _ := spawnPinned(t, f, "env-keeper", "")
	setRelaunchPolicy(t, f, "group_env", "reuse")
	setGroupEnv(t, "v2")

	clone := f.AsHuman().CloneWith(conv, map[string]any{"no_copy_conv": true, "group": "other", "keep_environment": true})
	require.Equalf(t, http.StatusOK, clone.Code, "clone body=%s", clone.Raw)
	policy, ok := f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v1", launchEnvValue(t, policy, "CREW_ENV"), "reuse: the kept environment is the one stored at the source's last launch")
}

// The sandbox profile contents: re-resolve (default) re-reads the source's
// profiles, as its resume would, and reuse keeps the stored contents.
func TestCloneRelaunchPolicy_SandboxProfileContents(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "shared",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "v1"}}})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("shared"))
	conv, _ := spawnPinned(t, f, "profile-source", "")

	editProfileEnv(t, "shared", "v2")
	clone := f.AsHuman().CloneFresh(conv)
	policy, ok := f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v2", profileEnvValue(t, policy), "re-resolve (default): the clone gets the edited profile")

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	clone = f.AsHuman().CloneFresh(conv)
	policy, ok = f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v1", profileEnvValue(t, policy), "reuse: the contents stored at the source's last launch")
}

// A clone always works, as a resume does: when the source's re-read profiles
// cannot be applied, it launches under the stored policy and says so.
func TestCloneRelaunchPolicy_UnusableProfilesFallBackToTheStoredPolicy(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "doomed",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "kept"}}})
	require.NoError(t, err)
	conv := spawnWithSandboxProfile(t, f, "doomed")
	_, err = db.DeleteSandboxProfile("doomed")
	require.NoError(t, err)

	clone := f.AsHuman().CloneFresh(conv)
	policy, ok := f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "kept", profileEnvValue(t, policy), "the stored policy")
	assert.Contains(t, string(clone.Raw), "launched under the sandbox policy stored at the source's last launch")
}

// Under reuse, which profiles apply is still read again, as on resume: a
// changed assignment brings the new profile's current contents, and says so.
func TestCloneRelaunchPolicy_ReuseFollowsAChangedAssignment(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	for name, value := range map[string]string{"shared": "v1", "other": "other"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name,
			Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: value}}})
		require.NoError(t, err)
	}
	require.NoError(t, db.SetGlobalSandboxProfile("shared"))
	conv, _ := spawnPinned(t, f, "assigned-source", "")
	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	require.NoError(t, db.SetGlobalSandboxProfile("other"))

	clone := f.AsHuman().CloneFresh(conv)
	policy, ok := f.World.SpawnSandboxPolicy(clone.NewConv)
	require.True(t, ok)
	assert.Equal(t, "other", profileEnvValue(t, policy))
	assert.Contains(t, string(clone.Raw), "so their current contents apply")
}
