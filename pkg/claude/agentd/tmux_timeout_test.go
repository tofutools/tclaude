package agentd

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tmuxTimeoutHelperMarker = "tclaude-tmux-timeout-helper"

func TestRunCommandWithTimeoutCapturesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell assertion is Unix-only")
	}
	err := runCommandWithTimeout(exec.Command("sh", "-c", "echo pane-missing >&2; exit 1"), time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pane-missing")
}

func TestRunCommandWithTimeoutKillsHungProcess(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestRunCommandWithTimeoutHelperProcess$",
		"--", tmuxTimeoutHelperMarker, readyPath,
	)
	timerStarted := make(chan time.Duration, 1)
	timeoutFired := make(chan time.Time, 1)
	done := make(chan struct{})
	var gotErr error
	go func() {
		gotErr = runCommandWithTimeoutTimer(cmd, 10*time.Millisecond, func(timeout time.Duration) (<-chan time.Time, func()) {
			timerStarted <- timeout
			return timeoutFired, func() {}
		})
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case timeoutFired <- time.Time{}:
		default:
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	})

	select {
	case gotTimeout := <-timerStarted:
		assert.Equal(t, 10*time.Millisecond, gotTimeout)
	case <-done:
		t.Fatalf("command returned before its timeout was armed: %v", gotErr)
	case <-time.After(10 * time.Second):
		t.Fatal("command did not start and arm its timeout")
	}
	waitForTmuxTimeoutHelper(t, readyPath, done)
	timeoutFired <- time.Time{}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("command did not return after its timeout fired")
	}

	require.ErrorIs(t, gotErr, errTmuxCommandTimeout)
	// ProcessState is populated only by Wait, so its presence proves the child
	// was reaped before runCommandWithTimeoutTimer returned. Exited reports false
	// for signal termination on Unix and is therefore not the right assertion.
	require.NotNil(t, cmd.ProcessState)
	assert.False(t, cmd.ProcessState.Success(), "timed-out helper exited successfully instead of being killed")
	assert.NotZero(t, cmd.ProcessState.ExitCode(), "timed-out helper reported a successful exit code")
}

func TestRunCommandWithTimeoutHelperProcess(t *testing.T) {
	args := flag.Args()
	if len(args) != 2 || args[0] != tmuxTimeoutHelperMarker {
		return
	}
	if err := os.WriteFile(args[1], []byte("ready"), 0o600); err != nil {
		t.Fatalf("write ready marker: %v", err)
	}
	time.Sleep(time.Hour)
	t.Fatal("timeout helper was not killed")
}

func waitForTmuxTimeoutHelper(t *testing.T, readyPath string, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		var ready []byte
		ready, lastErr = os.ReadFile(readyPath)
		if lastErr == nil && string(ready) == "ready" {
			return
		}
		select {
		case <-done:
			t.Fatalf("timeout helper exited before becoming ready (read error: %v)", lastErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("timeout helper did not become ready: %v", lastErr)
}
