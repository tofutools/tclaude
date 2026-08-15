//go:build linux

package agentd

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestOpenCodeManagedServerUsesPreparedResourceCgroupAtClone(t *testing.T) {
	cmd := exec.Command("/bin/true")
	closeFD, err := configureOpenCodeResourceCgroup(cmd, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(closeFD)
	require.NotNil(t, cmd.SysProcAttr)
	assert.True(t, cmd.SysProcAttr.UseCgroupFD)
}

func TestManagedOpenCodeExternalResourceCgroupLaunchUsesTmuxWrapper(t *testing.T) {
	runtime := db.OpenCodeRuntime{
		SessionID: "managed-external", ResourceCgroupDir: "/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-test",
	}
	command := openCodeTmuxLaunchCommand(runtime, "/opt/opencode",
		[]string{"serve", "--port", "43210"}, []string{"HOME=/srv/agent"}, nil)
	assert.Contains(t, command, "session resource-limit-exec")
	assert.Contains(t, command, "--cgroup-dir")
	assert.Contains(t, command, "--shared-boundary",
		"the agentd-owned boundary must survive a failed port attempt")
	assert.Contains(t, command, "tclaude-tmux.service/tclaude-test")
	assert.Contains(t, command, "'env HOME=/srv/agent /opt/opencode serve --port 43210'")
}

func TestManagedOpenCodeTmuxLaunchUsesExplicitBashArgv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runtime := db.OpenCodeRuntime{
		SessionID: "managed-external", ResourceCgroupDir: "/sys/fs/cgroup/tclaude-test",
	}
	args, cleanup, err := openCodeTmuxLaunchArgs(runtime, "/opt/opencode",
		[]string{"serve", "--hostname", "127.0.0.1"}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	require.Len(t, args, 2)
	assert.Equal(t, "/bin/bash", args[0])
	assert.Contains(t, args[1], "launch-scripts")
	raw, err := os.ReadFile(args[1])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "session resource-limit-exec")
	assert.Contains(t, string(raw), "--hostname 127.0.0.1")
}

func TestManagedOpenCodeSandboxLaunchCapturesStderrOutsideTmuxPane(t *testing.T) {
	handshake := &openCodeTmuxHandshake{
		statusPath: "/private/authority", gatePath: "/private/gate",
		stderrPath: "/private/stderr",
	}
	command := openCodeTmuxLaunchCommand(db.OpenCodeRuntime{}, "/opt/tclaude",
		[]string{"opencode-unix-launch"}, nil, handshake)

	assert.Contains(t, command, "3>/private/authority")
	assert.Contains(t, command, "4</private/gate")
	assert.Contains(t, command, "2>/private/stderr")
	assert.True(t, strings.LastIndex(command, "2>/private/stderr") >
		strings.LastIndex(command, "resource-limit-exec"),
		"stderr capture must wrap the resource-limit launcher")
}

func TestManagedOpenCodeLoopbackLaunchCapturesStderrWithoutHandshake(t *testing.T) {
	launchFiles := &openCodeTmuxHandshake{stderrPath: "/private/stderr"}
	command := openCodeTmuxLaunchCommand(db.OpenCodeRuntime{}, "/opt/opencode",
		[]string{"serve"}, nil, launchFiles)

	assert.NotContains(t, command, "3>")
	assert.NotContains(t, command, "4<")
	assert.Contains(t, command, "2>/private/stderr")
}
