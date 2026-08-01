//go:build linux

package agentd

import (
	"os/exec"
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
	assert.Contains(t, command, "tclaude-tmux.service/tclaude-test")
	assert.Contains(t, command, "'env HOME=/srv/agent /opt/opencode serve --port 43210'")
}
