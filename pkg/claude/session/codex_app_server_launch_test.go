package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestVerifyCodexAppServerLaunchVersionProbesExactLaunchExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	const generation = "0123456789abcdef"
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: generation, LaunchID: "launch", AgentID: "agent",
		SocketPath: filepath.Join(home, "app.sock"), State: db.CodexAppServerWarming,
	}))

	oldProbe := codexAppServerVersionProbe
	var probed string
	codexAppServerVersionProbe = func(executable, _ string, _ map[string]string) ([]byte, error) {
		probed = executable
		return []byte("codex-cli 0.147.2\n"), nil
	}
	t.Cleanup(func() { codexAppServerVersionProbe = oldProbe })

	const pinned = "/strict-home/staged/codex"
	err := verifyCodexAppServerLaunchVersion(&NewParams{
		CodexAppServer: true, CodexAppServerGeneration: generation,
	}, pinned, home, []sandboxpolicy.EnvironmentEntry{{Name: "PATH", Value: "/agentd-path-must-not-win"}})
	require.NoError(t, err)
	assert.Equal(t, pinned, probed)
	runtime, err := db.GetCodexAppServerRuntime(generation)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, "0.147.2", runtime.CodexVersion)
}

func TestVerifyCodexAppServerLaunchVersionFailsClosedAndMarksUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	const generation = "0123456789abcdef"
	require.NoError(t, db.UpsertCodexAppServerRuntime(db.CodexAppServerRuntime{
		Generation: generation, LaunchID: "launch", AgentID: "agent",
		SocketPath: filepath.Join(home, "app.sock"), State: db.CodexAppServerWarming,
	}))
	oldProbe := codexAppServerVersionProbe
	codexAppServerVersionProbe = func(string, string, map[string]string) ([]byte, error) {
		return []byte("codex-cli 0.148.0\n"), nil
	}
	t.Cleanup(func() { codexAppServerVersionProbe = oldProbe })

	err := verifyCodexAppServerLaunchVersion(&NewParams{
		CodexAppServer: true, CodexAppServerGeneration: generation,
	}, "/pinned/codex", home, nil)
	require.Error(t, err)
	runtime, getErr := db.GetCodexAppServerRuntime(generation)
	require.NoError(t, getErr)
	require.NotNil(t, runtime)
	assert.Equal(t, db.CodexAppServerUnavailable, runtime.State)
}

func TestCodexAppServerGenerationJoinsTclaudeLayerPrivateWriteContract(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	owner := filepath.Join(home, ".tclaude", "api", "codex", "0123456789abcdef")
	generation := filepath.Join(owner, "fedcba9876543210")
	require.NoError(t, os.MkdirAll(generation, 0o700))
	require.NoError(t, os.Chmod(owner, 0o700))
	require.NoError(t, os.Chmod(generation, 0o700))
	params := &NewParams{
		CodexAppServer: true, CodexAppServerGeneration: filepath.Base(generation),
		CodexAppServerSocket:       filepath.Join(generation, "app.sock"),
		CodexAppServerURL:          "ws://127.0.0.1:43210",
		CodexAppServerRelayURL:     "ws://127.0.0.1:43211",
		CodexAppServerTokenSHA256:  strings.Repeat("ab", 32),
		CodexAppServerTokenHandoff: filepath.Join(generation, "tui-capability.handoff"),
		CodexAppServerPIDFile:      filepath.Join(generation, "server.pid"),
		CodexAppServerLogFile:      filepath.Join(generation, "server.log"),
	}
	privateDir, err := codexAppServerPrivateWriteDir(params)
	require.NoError(t, err)
	require.NotNil(t, privateDir)
	assert.Equal(t, TclaudeLayerPrivateWriteDir{Parent: owner, Current: generation}, *privateDir)
	assert.Equal(t, 43210, codexAppServerLoopbackPort(params),
		"the daemon-minted listener must be threaded into the Darwin Seatbelt wrapper")
	assert.Equal(t, []int{43210, 43211}, codexAppServerLoopbackPorts(params),
		"both native server and TUI relay listeners must be admitted by the outer boundary")

	workspace := filepath.Join(home, "work")
	stateRoot := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CodexName, Cwd: workspace, StateRoot: stateRoot,
		PrivateWriteDirs: []TclaudeLayerPrivateWriteDir{*privateDir},
	})
	require.NoError(t, err)
	assert.Equal(t, []TclaudeLayerPrivateWriteDir{{Parent: owner, Current: generation}},
		spec.Contract.PrivateWriteDirs)
}
