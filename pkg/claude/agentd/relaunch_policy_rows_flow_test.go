package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Launch slice 9 (tcl-mx74na): the re-resolve | reuse rows of the relaunch
// policy (group env, group remote-control policy, sandbox profile contents)
// decide what a resume and a reincarnation read. Re-resolve, the default,
// reads the current value; reuse keeps the one stored at the last launch.

// resumeForPolicy takes every pane offline and resumes conv.
func resumeForPolicy(t *testing.T, f *testharness.Flow, conv string) testharness.ResumeResp {
	t.Helper()
	for _, session := range f.World.Tmux.Sessions() {
		f.MarkOffline(session)
	}
	r := f.AsHuman().Resume(conv)
	f.AssertResumeSpawned(r)
	return r
}

func launchEnvValue(t *testing.T, snap *sandboxpolicy.Snapshot, name string) string {
	t.Helper()
	require.NotNil(t, snap)
	for _, entry := range snap.LaunchEnvironment {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}

func setGroupEnv(t *testing.T, value string) {
	t.Helper()
	_, err := db.SetAgentGroupEnvironment("crew", []db.SandboxEnvironmentEntry{{Name: "CREW_ENV", Value: value}})
	require.NoError(t, err)
}

func TestRelaunchPolicy_GroupEnvOnResume(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	setGroupEnv(t, "v1")
	conv, _ := spawnPinned(t, f, "env-worker", "")

	setGroupEnv(t, "v2")
	r := resumeForPolicy(t, f, conv)
	policy, ok := f.World.SpawnSandboxPolicy(conv)
	require.True(t, ok)
	assert.Equal(t, "v2", launchEnvValue(t, policy, "CREW_ENV"), "re-resolve (default) reads the group's current env")
	assert.Contains(t, r.Changes, "group environment: changed CREW_ENV")

	setRelaunchPolicy(t, f, "group_env", "reuse")
	setGroupEnv(t, "v3")
	r = resumeForPolicy(t, f, conv)
	policy, _ = f.World.SpawnSandboxPolicy(conv)
	assert.Equal(t, "v2", launchEnvValue(t, policy, "CREW_ENV"), "reuse keeps the env stored at the last launch")
	assert.Empty(t, r.Changes)

	setRelaunchPolicy(t, f, "group_env", "re-resolve")
	resumeForPolicy(t, f, conv)
	policy, _ = f.World.SpawnSandboxPolicy(conv)
	assert.Equal(t, "v3", launchEnvValue(t, policy, "CREW_ENV"), "back to re-resolve picks up the edit")
}

func TestRelaunchPolicy_GroupEnvOnReincarnate(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	setGroupEnv(t, "v1")
	conv, _ := spawnPinned(t, f, "env-reinc", "")

	setRelaunchPolicy(t, f, "group_env", "reuse")
	setGroupEnv(t, "v2")
	r := f.AsHuman().Reincarnate(conv, "carry on")
	policy, ok := f.World.SpawnSandboxPolicy(r.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v1", launchEnvValue(t, policy, "CREW_ENV"), "reuse keeps the stored env across a reincarnation")

	setRelaunchPolicy(t, f, "group_env", "re-resolve")
	r = f.AsHuman().Reincarnate(r.NewConv, "carry on")
	policy, ok = f.World.SpawnSandboxPolicy(r.NewConv)
	require.True(t, ok)
	assert.Equal(t, "v2", launchEnvValue(t, policy, "CREW_ENV"), "re-resolve reads the current env")
}

func profileEnvValue(t *testing.T, snap *sandboxpolicy.Snapshot) string {
	t.Helper()
	require.NotNil(t, snap)
	for _, entry := range snap.Effective.Environment {
		if entry.Name == "PROFILE_ENV" {
			return entry.Value
		}
	}
	return ""
}

func editProfileEnv(t *testing.T, name, value string) {
	t.Helper()
	profile, err := db.GetSandboxProfile(name)
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: value}}
	require.NoError(t, db.UpdateSandboxProfile(profile))
}

func TestRelaunchPolicy_SandboxProfileContentsOnResume(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "shared",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "v1"}}})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("shared"))
	conv, _ := spawnPinned(t, f, "profile-worker", "")

	editProfileEnv(t, "shared", "v2")
	resumeForPolicy(t, f, conv)
	policy, ok := f.World.SpawnSandboxPolicy(conv)
	require.True(t, ok)
	assert.Equal(t, "v2", profileEnvValue(t, policy), "re-resolve (default) reads the edited profile")

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	editProfileEnv(t, "shared", "v3")
	r := resumeForPolicy(t, f, conv)
	policy, _ = f.World.SpawnSandboxPolicy(conv)
	assert.Equal(t, "v2", profileEnvValue(t, policy), "reuse keeps the contents stored at the last launch")
	assert.Empty(t, r.Changes)

	// Which profile applies is a setting of its own and is always re-read:
	// a new assignment brings the new profile's current contents, and says so.
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "other",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "other"}}})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("other"))
	r = resumeForPolicy(t, f, conv)
	policy, _ = f.World.SpawnSandboxPolicy(conv)
	assert.Equal(t, "other", profileEnvValue(t, policy))
	assert.Contains(t, r.Changes, `sandbox profiles: global "shared" → global "other"`)
	require.Len(t, r.Warnings, 1)
	assert.Contains(t, r.Warnings[0], "so their current contents apply")
}

func TestRelaunchPolicy_SandboxProfileContentsReuseSurvivesADeletedProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "doomed",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_ENV", Value: "kept"}}})
	require.NoError(t, err)
	conv := spawnWithSandboxProfile(t, f, "doomed")

	setRelaunchPolicy(t, f, "sandbox_profile_contents", "reuse")
	_, err = db.DeleteSandboxProfile("doomed")
	require.NoError(t, err)
	r := resumeForPolicy(t, f, conv)
	policy, _ := f.World.SpawnSandboxPolicy(conv)
	assert.Equal(t, "kept", profileEnvValue(t, policy), "reuse keeps the stored contents, so a deleted profile does not matter")
	assert.Empty(t, r.Warnings, "nothing fell back: reuse asked for the stored contents")
}

func spawnWithSandboxProfile(t *testing.T, f *testharness.Flow, profile string) string {
	t.Helper()
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "named-" + profile, "cwd": t.TempDir(), "sandbox_profile": profile})
	require.Equalf(t, 200, spawn.Code, "spawn body=%s", spawn.Raw)
	return spawn.ConvID
}

func setGroupRemoteControl(t *testing.T, policy *bool) {
	t.Helper()
	_, err := db.SetAgentGroupRemoteControl("crew", policy)
	require.NoError(t, err)
}

func resumedRemoteControl(t *testing.T, f *testharness.Flow, conv string) (bool, testharness.ResumeResp) {
	t.Helper()
	r := resumeForPolicy(t, f, conv)
	got, ok := f.World.SpawnRemoteControl(conv)
	require.True(t, ok)
	// An armed resume tags the new row in the background; let it finish
	// before the next resume takes the pane offline.
	agentd.WaitForBackgroundForTest()
	return got, r
}

func TestRelaunchPolicy_GroupRemoteControlOnResume(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	conv, _ := spawnPinned(t, f, "rc-worker", "")
	on, off := true, false

	setRelaunchPolicy(t, f, "group_remote_control", "reuse")
	setGroupRemoteControl(t, &on)
	got, r := resumedRemoteControl(t, f, conv)
	assert.False(t, got, "reuse keeps the state of the last launch")
	assert.Empty(t, r.Changes)

	setRelaunchPolicy(t, f, "group_remote_control", "re-resolve")
	got, r = resumedRemoteControl(t, f, conv)
	assert.True(t, got, "re-resolve applies the group's current policy")
	assert.Contains(t, r.Changes, "remote control: off → on (group policy)")

	setGroupRemoteControl(t, &off)
	got, _ = resumedRemoteControl(t, f, conv)
	assert.False(t, got, "a policy that turns it off applies too")
}

func TestRelaunchPolicy_GroupRemoteControlKeepsAnExplicitFlag(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "rc-explicit", "cwd": t.TempDir(), "remote_control": false})
	require.Equalf(t, 200, spawn.Code, "spawn body=%s", spawn.Raw)
	on := true
	setGroupRemoteControl(t, &on)
	got, r := resumedRemoteControl(t, f, spawn.ConvID)
	assert.False(t, got, "an explicit --remote-control=false stays under re-resolve")
	assert.Empty(t, r.Changes)
}

func TestRelaunchPolicy_GroupRemoteControlPreLayersAgentKeepsItsValue(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	conv, agentID := spawnPinned(t, f, "rc-legacy", "")
	// An agent spawned before launch layers were recorded.
	require.NoError(t, db.SetAgentLaunchLayers(agentID, nil))
	on := true
	setGroupRemoteControl(t, &on)
	got, r := resumedRemoteControl(t, f, conv)
	assert.False(t, got, "with no record of an explicit value the stored one is kept")
	require.NotEmpty(t, r.Warnings)
	assert.Contains(t, r.Warnings[len(r.Warnings)-1], "predates launch records")
}
