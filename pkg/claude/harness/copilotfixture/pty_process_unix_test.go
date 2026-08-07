//go:build linux || darwin

package copilotfixture_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TestRunPTYStopsDescendantsBeforeReturning pins the cleanup behind TCL-1045.
//
// Copilot starts helpers in its PTY session. Killing only the CLI parent left
// those helpers free to keep writing into the fixture's t.TempDir; on macOS,
// testing's concurrent RemoveAll then failed with "directory not empty" even
// though every permission assertion had passed.
//
// This fake closes every terminal descriptor before starting its writer, so
// RunPTY's PTY-reader drain cannot accidentally synchronize with it. The PID
// assertion is deliberately immediate: a stable heartbeat sampled later could
// miss a write in the exact window between RunPTY returning and the first Stat.
func TestRunPTYStopsDescendantsBeforeReturning(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "descendant-heartbeat")
	fakeCopilot(t, `
(
  exec </dev/null >/dev/null 2>&1
  while :; do
    printf x >> "$HEARTBEAT"
    sleep 0.02
  done
) &
echo "descendant=$!"
echo ready
sleep 60`)

	run := fakeRunOptions(t)
	run.ExtraEnv = []string{"HEARTBEAT=" + heartbeat}
	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions:   run,
		Deadline:     30 * time.Second,
		BlockedAfter: 3 * time.Second,
	})

	var descendantPID int
	for _, field := range strings.Fields(res.TranscriptText()) {
		if value, ok := strings.CutPrefix(field, "descendant="); ok {
			descendantPID, _ = strconv.Atoi(strings.TrimSpace(value))
			break
		}
	}
	if descendantPID > 0 {
		t.Cleanup(func() {
			process, err := os.FindProcess(descendantPID)
			if err == nil {
				_ = process.Kill()
			}
		})
	}

	require.True(t, res.Blocked, "the fake must exercise the early-stop path")
	require.NotZero(t, descendantPID, "the fake must disclose its background writer pid")
	heartbeatInfo, err := os.Stat(heartbeat)
	require.NoError(t, err)
	require.Positive(t, heartbeatInfo.Size(), "the descendant must write before teardown")
	require.ErrorIs(t, syscall.Kill(descendantPID, 0), syscall.ESRCH,
		"RunPTY returned while its detached-IO descendant still existed")

	time.Sleep(250 * time.Millisecond)
	after, err := os.Stat(heartbeat)
	require.NoError(t, err)
	require.Equal(t, heartbeatInfo.Size(), after.Size(),
		"a descendant mutated fixture state after RunPTY returned")
}
