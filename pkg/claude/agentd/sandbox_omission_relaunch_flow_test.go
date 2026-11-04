package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// omissionFixture is a group assigned an environment-only sandbox profile,
// and a second profile a spawn can name explicitly.
func omissionFixture(t *testing.T) *testharness.Flow {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("crew")
	for _, name := range []string{"group-env", "explicit-env"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{
			Name: name, Environment: []db.SandboxEnvironmentEntry{{Name: "FROM_" + name[:5], Value: "yes"}},
		})
		require.NoError(t, err)
	}
	_, err := db.SetAgentGroupSandboxProfile("crew", "group-env")
	require.NoError(t, err)
	return f
}

// setRelaunchMode stands in for whatever later changes an offline agent's
// sandbox mode: the next resume launches with it.
func setRelaunchMode(t *testing.T, convID, mode string) {
	t.Helper()
	stored := storedRelaunch(t, convID)
	stored.HarnessBuiltinMode = &mode
	require.NoError(t, db.SetAgentRelaunchProfile(agentIDFor(t, convID), *stored))
}

func resumedSandbox(t *testing.T, f *testharness.Flow, spawn testharness.SpawnResp) *sandboxpolicy.Snapshot {
	t.Helper()
	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	persisted, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	return persisted
}

func appliedNames(s *sandboxpolicy.Snapshot) []string {
	var out []string
	for _, a := range s.Applied {
		out = append(out, a.Name)
	}
	return out
}

// An omission the launch mode forced (Codex's raw no-sandbox mode carries no
// tclaude sandbox profile) is recorded as derived. Once the agent's mode can
// carry profiles again, its next resume re-reads the group's assignment as it
// is now, and the profile the spawn named.
func TestSandboxOmission_ADerivedOmissionIsReResolvedWhenTheModeChanges(t *testing.T) {
	f := omissionFixture(t)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "danger-full-access", "sandbox_profile": "explicit-env",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	born, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, born)
	require.True(t, born.ProfilesOmitted)
	require.True(t, born.ProfilesOmittedDerived, "the mode forced this omission")

	setRelaunchMode(t, spawn.ConvID, "tclaude-agent")
	resumed := resumedSandbox(t, f, spawn)
	assert.False(t, resumed.ProfilesOmitted, "the new mode carries profiles, so they apply")
	assert.ElementsMatch(t, []string{"group-env", "explicit-env"}, appliedNames(resumed))
	assert.Contains(t, resumed.Effective.Environment, db.SandboxEnvironmentEntry{Name: "FROM_group", Value: "yes"})
	assert.Contains(t, resumed.Effective.Environment, db.SandboxEnvironmentEntry{Name: "FROM_expli", Value: "yes"})
}

// While the mode still cannot carry profiles, a derived omission stays, and
// stays derived.
func TestSandboxOmission_ADerivedOmissionStaysWhileTheModeStays(t *testing.T) {
	f := omissionFixture(t)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "danger-full-access",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	resumed := resumedSandbox(t, f, spawn)
	assert.True(t, resumed.ProfilesOmitted)
	assert.True(t, resumed.ProfilesOmittedDerived)
	assert.Empty(t, resumed.Applied)
}

// An omission the spawn asked for is the agent's own choice: no relaunch
// brings the group's or the global assignment back, whatever its mode.
func TestSandboxOmission_AnExplicitOmissionIsKept(t *testing.T) {
	f := omissionFixture(t)
	require.NoError(t, db.SetGlobalSandboxProfile("explicit-env"))
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "harness": "codex", "sandbox": "danger-full-access", "omit_sandbox_profiles": true,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	born, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, born)
	require.True(t, born.ProfilesOmitted)
	require.False(t, born.ProfilesOmittedDerived, "the spawn asked for this omission")

	setRelaunchMode(t, spawn.ConvID, "tclaude-agent")
	resumed := resumedSandbox(t, f, spawn)
	assert.True(t, resumed.ProfilesOmitted)
	assert.False(t, resumed.ProfilesOmittedDerived)
	assert.Empty(t, resumed.Applied)
}
