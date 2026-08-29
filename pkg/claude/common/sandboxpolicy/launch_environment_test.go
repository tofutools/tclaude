package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeEnvironmentLaterTiersWin(t *testing.T) {
	got, err := MergeEnvironment(
		[]EnvironmentEntry{{Name: "A", Value: "sandbox"}, {Name: "B", Value: "sandbox"}},
		[]EnvironmentEntry{{Name: "A", Value: "group"}},
		[]EnvironmentEntry{{Name: "A", Value: "spawn"}, {Name: "C", Value: "spawn"}},
	)
	require.NoError(t, err)
	assert.Equal(t, []EnvironmentEntry{{Name: "A", Value: "spawn"}, {Name: "B", Value: "sandbox"}, {Name: "C", Value: "spawn"}}, got)
}

func TestLaunchEnvironmentSurvivesSnapshotRevalidationAndUnconfinedLaunch(t *testing.T) {
	snapshot := EmptySnapshot()
	snapshot.LaunchEnvironment = []EnvironmentEntry{{Name: "COMMON", Value: "frozen"}}
	validated, err := RevalidateSnapshot(snapshot)
	require.NoError(t, err)
	assert.Equal(t, snapshot.LaunchEnvironment, validated.LaunchEnvironment)
	assert.Equal(t, snapshot.LaunchEnvironment, UnconfinedLaunchSnapshot(snapshot).LaunchEnvironment)
}

func TestEnvironmentForLaunchPreservesTrustedGeneratedBindings(t *testing.T) {
	snapshot := EmptySnapshot()
	// PATH and CODEX_HOME cannot be operator-authored, but launch adapters add
	// trusted bindings after the authored profile has been validated.
	snapshot.Effective.Environment = []EnvironmentEntry{
		{Name: "PATH", Value: "/generated/bin"},
		{Name: "CODEX_HOME", Value: "/generated/codex"},
		{Name: "COMMON", Value: "sandbox"},
		{Name: "GOCACHE", Value: "/private/cache"},
		{Name: "GIT_SSH_COMMAND", Value: "generated ssh command"},
	}
	snapshot.Effective.AgentDirectories = []string{"GOCACHE"}
	snapshot.Effective.Provenance.Environment["GIT_SSH_COMMAND"] = ProfileSource{
		Scope: ScopeExplicit, Profile: "__tclaude_codex_ssh_workaround",
	}
	snapshot.LaunchEnvironment = []EnvironmentEntry{
		{Name: "COMMON", Value: "spawn"},
		{Name: "GOCACHE", Value: "/operator/cache"},
		{Name: "GIT_SSH_COMMAND", Value: "operator ssh command"},
		{Name: "TEAM", Value: "platform"},
	}

	assert.Equal(t, []EnvironmentEntry{
		{Name: "PATH", Value: "/generated/bin"},
		{Name: "CODEX_HOME", Value: "/generated/codex"},
		{Name: "COMMON", Value: "spawn"},
		{Name: "GOCACHE", Value: "/private/cache"},
		{Name: "GIT_SSH_COMMAND", Value: "generated ssh command"},
		{Name: "TEAM", Value: "platform"},
	}, EnvironmentForLaunch(&snapshot))
}
