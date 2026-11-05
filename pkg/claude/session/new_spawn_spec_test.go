package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Only a constructed-root tclaude-layer launch carries the agentd socket it
// selected into the launch shell; every other launch starts from the
// profile's environment alone.
func TestLaunchShellEnvironmentCarriesTheSelectedSocketOnlyBehindAConstructedRoot(t *testing.T) {
	env := map[string]string{agentipc.SocketEnv: "/run/agentd.sock"}

	got := launchShellEnvironment(nil, true, sandboxpolicy.RootConstructed, env)
	assert.Equal(t, map[string]string{agentipc.SocketEnv: "/run/agentd.sock"}, got)

	assert.Empty(t, launchShellEnvironment(nil, false, sandboxpolicy.RootConstructed, env), "no outer layer")
	assert.Empty(t, launchShellEnvironment(nil, true, sandboxpolicy.RootHostInherited, env), "host root")
	assert.Empty(t, launchShellEnvironment(nil, true, sandboxpolicy.RootConstructed, map[string]string{}), "no socket selected")
}

// pi's live-status extension: the spec points at it, a walled pi gets its own
// agent dir and the extension's dir as a read path, and behind a constructed
// root the extension reaches tclaude where the sandbox puts it. A harness
// without an extension is left as built.
func TestFinishHarnessSpawnSpec(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	// pi resolves its agent dir from this before HOME; a run inside a pi pane
	// must not reach the real one.
	t.Setenv(harness.PiAgentDirEnvVar, t.TempDir())

	claude := harness.MustGet(harness.DefaultName)
	spec := harness.SpawnSpec{SessionID: "conv-claude"}
	paths, err := finishHarnessSpawnSpec(claude, &spec, &NewParams{ManagedInstructionsRoot: "/managed"},
		true, sandboxpolicy.RootConstructed, "")
	require.NoError(t, err)
	assert.Empty(t, paths)
	assert.Equal(t, harness.SpawnSpec{SessionID: "conv-claude"}, spec, "claude registers nothing here")

	pi := harness.MustGet(harness.PiName)
	if pi.StatusExtension == nil {
		t.Skip("pi has no status extension in this build")
	}
	spec = harness.SpawnSpec{SessionID: "conv-pi", TclaudeExecutable: "/usr/bin/tclaude"}
	paths, err = finishHarnessSpawnSpec(pi, &spec, &NewParams{ManagedInstructionsRoot: "/managed"},
		true, sandboxpolicy.RootConstructed, "")
	require.NoError(t, err)
	assert.Equal(t, "/managed", spec.ManagedInstructionsRoot)
	assert.NotEmpty(t, spec.HarnessStateDir, "a walled pi runs against its own agent dir")
	require.NotEmpty(t, spec.StatusExtensionPath)
	assert.Equal(t, []string{filepath.Dir(spec.StatusExtensionPath)}, paths)
	assert.Equal(t, tclaudeLayerConstructedRootTclaudePath, spec.TclaudeExecutable)

	spec = harness.SpawnSpec{SessionID: "conv-pi-host", TclaudeExecutable: "/usr/bin/tclaude"}
	paths, err = finishHarnessSpawnSpec(pi, &spec, &NewParams{}, false, sandboxpolicy.RootHostInherited, "")
	require.NoError(t, err)
	assert.Empty(t, paths, "without tclaude's sandbox nothing needs mounting")
	assert.Empty(t, spec.HarnessStateDir)
	assert.Equal(t, "/usr/bin/tclaude", spec.TclaudeExecutable)
}
