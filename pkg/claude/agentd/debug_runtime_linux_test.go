//go:build linux

package agentd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveAgentProcessUsesValidatedHostHarnessPID(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	stub := filepath.Join(t.TempDir(), "codex")
	bytes, err := os.ReadFile(self)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(stub, bytes, 0o700))
	cmd := exec.Command(stub, "-test.run=^TestDebugRuntimeHarnessHelper$", "-count=1")
	cmd.Env = append(os.Environ(), "TCLAUDE_DEBUG_RUNTIME_HELPER=1", "PATH=/debug/runtime/path")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	var got agentDebugLiveProcess
	require.Eventually(t, func() bool {
		got = observeAgentProcess(cmd.Process.Pid, "codex")
		return got.Status == "observed"
	}, 3*time.Second, 25*time.Millisecond)
	assert.Equal(t, cmd.Process.Pid, got.PID)
	assert.Equal(t, stub, got.ExecutablePath)
	assert.Equal(t, "/debug/runtime/path", got.PATH)
	assert.NotNil(t, got.UID)
	assert.NotNil(t, got.GID)
	assert.NotEmpty(t, got.UIDMap)
	assert.NotEmpty(t, got.GIDMap)
}

func TestDebugRuntimeHarnessHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_DEBUG_RUNTIME_HELPER") != "1" {
		t.Skip("helper subprocess only")
	}
	time.Sleep(10 * time.Second)
}
