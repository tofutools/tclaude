package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The explicit sandbox profile of an EXISTING agent can be changed through the
// same assignment as its implementation: the profile alone, the implementation
// alone, or both in one restart. These scenarios check the change survives into
// the launch that follows it, which is the only place it matters.

type sandboxProfileAssignWire struct {
	Implementation  string   `json:"sandbox_implementation"`
	Previous        string   `json:"previous_sandbox_implementation"`
	Profile         string   `json:"sandbox_profile"`
	Pending         bool     `json:"sandbox_profile_pending"`
	PreviousProfile *string  `json:"previous_sandbox_profile"`
	Omitted         bool     `json:"sandbox_profiles_omitted"`
	Unchangeable    string   `json:"sandbox_profile_unchangeable"`
	ProfileOptions  []string `json:"sandbox_profile_options"`
	Restart         string   `json:"restart"`
}

func postSandboxAssign(t *testing.T, f *testharness.Flow, convID string, body map[string]any) (sandboxProfileAssignWire, int, string) {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+convID+"/sandbox-impl", body)))
	var wire sandboxProfileAssignWire
	if rec.Code == http.StatusOK {
		testharness.DecodeJSON(t, rec, &wire)
	}
	return wire, rec.Code, rec.Body.String()
}

func getSandboxAssign(t *testing.T, f *testharness.Flow, convID string) sandboxProfileAssignWire {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/agent/"+convID+"/sandbox-impl", nil)))
	require.Equalf(t, http.StatusOK, rec.Code, "show body=%s", rec.Body.String())
	var wire sandboxProfileAssignWire
	testharness.DecodeJSON(t, rec, &wire)
	return wire
}

func haveEnvProfile(t *testing.T, name, value string) db.SandboxEnvironmentEntry {
	t.Helper()
	entry := db.SandboxEnvironmentEntry{Name: "PROFILE_MARK", Value: value}
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name, Environment: []db.SandboxEnvironmentEntry{entry}})
	require.NoError(t, err)
	return entry
}

func explicitApplied(snapshot *sandboxpolicy.Snapshot) string {
	name := ""
	for _, applied := range snapshot.Applied {
		if applied.Scope == sandboxpolicy.ScopeExplicit {
			name = applied.Name
		}
	}
	return name
}

func TestSandboxProfileAssign_OfflineAgentRelaunchesUnderTheNewProfile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	oldMark := haveEnvProfile(t, "old-profile", "old")
	newMark := haveEnvProfile(t, "new-profile", "new")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	before := getSandboxAssign(t, f, spawn.ConvID)
	assert.Equal(t, "old-profile", before.Profile)
	assert.False(t, before.Pending)
	assert.Empty(t, before.Unchangeable)
	assert.ElementsMatch(t, []string{"old-profile", "new-profile"}, before.ProfileOptions)

	f.AsHuman().Stop(spawn.ConvID, false)
	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": "new-profile"})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Equal(t, "new-profile", wire.Profile)
	assert.True(t, wire.Pending, "recorded, not yet launched")
	require.NotNil(t, wire.PreviousProfile)
	assert.Equal(t, "old-profile", *wire.PreviousProfile)
	assert.Empty(t, wire.Previous, "a profile-only change leaves the implementation alone")
	assert.Equal(t, before.Implementation, wire.Implementation)

	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	assert.Empty(t, resume.Warnings)
	launched, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	assert.Contains(t, launched.Effective.Environment, newMark, "the relaunch runs under the new profile")
	assert.NotContains(t, launched.Effective.Environment, oldMark)
	assert.Equal(t, "new-profile", explicitApplied(launched))

	after := getSandboxAssign(t, f, spawn.ConvID)
	assert.Equal(t, "new-profile", after.Profile)
	assert.False(t, after.Pending, "the launch recorded it, so nothing is pending")
}

func TestSandboxProfileAssign_ClearsTheExplicitProfile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	oldMark := haveEnvProfile(t, "old-profile", "old")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": ""})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Empty(t, wire.Profile)

	f.AssertResumeSpawned(f.AsHuman().Resume(spawn.ConvID))
	launched, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	assert.NotContains(t, launched.Effective.Environment, oldMark)
	assert.Empty(t, explicitApplied(launched))
}

// Choosing the profile the agent already launched under again cancels a
// pending change rather than recording a second one.
func TestSandboxProfileAssign_ReassigningTheLaunchedProfileCancelsThePendingChange(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	haveEnvProfile(t, "old-profile", "old")
	haveEnvProfile(t, "new-profile", "new")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": "new-profile"})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Equal(t, "old-profile", wire.Profile)
	assert.False(t, wire.Pending)
}

func TestSandboxProfileAssign_RefusesAnUnknownProfileAndWritesNothing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	haveEnvProfile(t, "old-profile", "old")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": "no-such-profile"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "invalid_sandbox_profile")
	wire := getSandboxAssign(t, f, spawn.ConvID)
	assert.Equal(t, "old-profile", wire.Profile)
	assert.False(t, wire.Pending)

	_, code, body = postSandboxAssign(t, f, spawn.ConvID, map[string]any{})
	assert.Equal(t, http.StatusBadRequest, code, "an empty request changes nothing: %s", body)
}

// Both at once, on a running idle agent: one restart, and the relaunch carries
// both the new implementation and the new profile.
func TestSandboxProfileAssign_RestartAppliesImplementationAndProfileTogether(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	haveEnvProfile(t, "old-profile", "old")
	newMark := haveEnvProfile(t, "new-profile", "new")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)

	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{
		"implementation":  string(sandboxpolicy.ImplementationHarnessBuiltin),
		"sandbox_profile": "new-profile",
		"restart":         true,
	})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Equal(t, "resumed", wire.Restart)
	assert.Equal(t, "new-profile", wire.Profile)
	assert.False(t, wire.Pending, "the restart already launched under it")
	launched, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	assert.Contains(t, launched.Effective.Environment, newMark)
	assert.Equal(t, "new-profile", explicitApplied(launched))
}

// An agent spawned with every sandbox profile omitted can take them back: a
// profile assignment lifts the omission, and the relaunch resolves the group
// tier and the assigned explicit one.
func TestSandboxProfileAssign_LiftsTheOmissionOfAnAgentThatOmitsProfiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	newMark := haveEnvProfile(t, "new-profile", "new")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "omit_sandbox_profiles": true})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	before := getSandboxAssign(t, f, spawn.ConvID)
	assert.True(t, before.Omitted)
	assert.Empty(t, before.Unchangeable, "an omission is changeable")

	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": "new-profile"})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.False(t, wire.Omitted)
	assert.Equal(t, "new-profile", wire.Profile)
	assert.True(t, wire.Pending)

	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	launched, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	assert.False(t, launched.ProfilesOmitted)
	assert.Contains(t, launched.Effective.Environment, newMark)
	assert.Equal(t, "new-profile", explicitApplied(launched))
	after := getSandboxAssign(t, f, spawn.ConvID)
	assert.False(t, after.Omitted)
	assert.False(t, after.Pending)
}

// "Default" lifts the omission too, with no explicit profile.
func TestSandboxProfileAssign_DefaultLiftsTheOmission(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "omit_sandbox_profiles": true})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": ""})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.False(t, wire.Omitted)
	assert.Empty(t, wire.Profile)

	f.AssertResumeSpawned(f.AsHuman().Resume(spawn.ConvID))
	launched, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	assert.False(t, launched.ProfilesOmitted)
	assert.False(t, getSandboxAssign(t, f, spawn.ConvID).Omitted)
}

// omit_sandbox_profiles takes an agent's profiles away, and the omission
// survives the relaunch.
func TestSandboxProfileAssign_OmitsTheProfilesOfAnAgent(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	oldMark := haveEnvProfile(t, "old-profile", "old")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "old-profile"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{
		"sandbox_profile": "old-profile", "omit_sandbox_profiles": true})
	assert.Equal(t, http.StatusBadRequest, code, "the two are exclusive: %s", body)

	wire, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"omit_sandbox_profiles": true})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.True(t, wire.Omitted)
	require.NotNil(t, wire.PreviousProfile)
	assert.Equal(t, "old-profile", *wire.PreviousProfile)

	f.AssertResumeSpawned(f.AsHuman().Resume(spawn.ConvID))
	launched, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	assert.True(t, launched.ProfilesOmitted)
	assert.NotContains(t, launched.Effective.Environment, oldMark)
	assert.Empty(t, explicitApplied(launched))
	assert.True(t, getSandboxAssign(t, f, spawn.ConvID).Omitted)
}

// An agent whose implementation cannot carry profiles cannot take them back
// without an implementation that can.
func TestSandboxProfileAssign_RefusesLiftingTheOmissionUnderImplementationOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	haveEnvProfile(t, "new-profile", "new")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "sandbox_implementation": string(sandboxpolicy.ImplementationOff)})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)
	require.True(t, getSandboxAssign(t, f, spawn.ConvID).Omitted)

	_, code, body := postSandboxAssign(t, f, spawn.ConvID, map[string]any{"sandbox_profile": "new-profile"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "cannot carry sandbox profiles")
}
