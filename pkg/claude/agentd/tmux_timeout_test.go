package agentd

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunCommandWithTimeoutCapturesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell assertion is Unix-only")
	}
	err := runCommandWithTimeout(exec.Command("sh", "-c", "echo pane-missing >&2; exit 1"), time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pane-missing")
}

func TestRunCommandWithTimeoutKillsHungProcess(t *testing.T) {
	cmd := exec.Command("sleep", "1")
	started := time.Now()
	err := runCommandWithTimeout(cmd, 10*time.Millisecond)
	require.ErrorIs(t, err, errTmuxCommandTimeout)
	assert.Less(t, time.Since(started), 500*time.Millisecond)
}
