package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShellHarnessRegistration(t *testing.T) {
	h, err := ResolveSpawnable(ShellName)
	require.NoError(t, err)
	assert.Equal(t, "Shell", h.DisplayName)
	assert.True(t, h.SupportsLaunchEnrollment())
	assert.True(t, h.WantsTmuxScrollback())
	assert.NotContains(t, SpawnBinaries(), "sh")
}

func TestShellSpawnerBuildCommand(t *testing.T) {
	t.Setenv("SHELL", "/bin/test shell")
	spawner := shellSpawner{}
	assert.Equal(t, `export A='b'; exec script -qefc 'exec '\''/bin/test shell'\'' -i' /dev/null`, spawner.BuildCommand(SpawnSpec{
		EnvExports: "export A='b'; ",
	}))
	assert.Equal(t, `exec '/bin/test shell' -c 'printf '\''hello world'\'''`,
		spawner.BuildCommand(SpawnSpec{InitialPrompt: "printf 'hello world'"}))
}

func TestShellCatalogRejectsAgentOnlyModelFields(t *testing.T) {
	h := MustGet(ShellName)
	_, err := h.Models.ValidateModel("gpt-5")
	assert.EqualError(t, err, "shell harness has no model")
	_, err = h.Models.ValidateEffort("high")
	assert.EqualError(t, err, "shell harness has no reasoning effort")
	assert.Equal(t, ShellSandboxOff, h.Sandbox.DefaultMode())
}
