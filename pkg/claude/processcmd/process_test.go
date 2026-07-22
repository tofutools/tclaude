package processcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestRuntimeVerbsRemainDiscoverableAndReturnNoEngine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, config.Save(&config.Config{
		Features: &config.FeaturesConfig{Processes: true},
	}))

	want := []string{"advance", "apply", "observe", "preview", "repair", "report", "resolve", "run", "runs", "show", "unblock", "verify", "worklist"}
	root := Cmd()
	for _, name := range want {
		command, _, err := root.Find([]string{name})
		require.NoError(t, err, name)
		assert.Equal(t, name, command.Name())
	}

	root = Cmd()
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetArgs([]string{"show", "legacy-run", "--store-root", t.TempDir()})
	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temporarily unavailable: no engine is installed")
}

func TestRuntimeVerbIsDiscoverableAndReturnsNoEngineByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := Cmd()
	assert.False(t, root.Hidden)
	command, _, err := root.Find([]string{"run"})
	require.NoError(t, err)
	assert.Equal(t, "run", command.Name())
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetArgs([]string{"run", "template.yaml"})
	err = root.Execute()
	require.Error(t, err)
	assert.Equal(t, "process runtime is temporarily unavailable: no engine is installed", err.Error())
}
