package agentd

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// listSessionsTmux swaps only what lifecycleSessionAlive touches: a
// list-sessions invocation with a scripted outcome. Everything else
// succeeds as a no-op.
type listSessionsTmux struct {
	script func() *exec.Cmd
}

func (l listSessionsTmux) Command(args ...string) *exec.Cmd {
	if args[0] == "list-sessions" {
		return l.script()
	}
	return exec.Command("true")
}

func (listSessionsTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// The incident this pins: the retired pane was the LAST live tmux session, so
// its death took the tmux server down with it, list-sessions started failing
// with tmux's dead-server message, and lifecycleSessionAlive read that as
// "unknown" — leaving waitForLifecycleTargetGone polling blind for the whole
// 10 s escalation deadline after the pane had already exited within 1 s. A
// dead server is a KNOWN all-offline state; only a genuinely unreadable
// listing (transient fault, timeout) may stay unknown.
func TestLifecycleSessionAlive_DeadServerIsKnownOffline(t *testing.T) {
	tests := []struct {
		name      string
		script    func() *exec.Cmd
		wantAlive bool
		wantKnown bool
	}{
		{
			// tmux 3.x when the socket file is gone (server exited with its
			// last session) — the spelling observed in the incident.
			name: "socket gone",
			script: func() *exec.Cmd {
				return exec.Command("sh", "-c",
					"echo 'error connecting to /tmp/tmux-1000/tclaude (No such file or directory)' >&2; exit 1")
			},
			wantAlive: false, wantKnown: true,
		},
		{
			// The other spelling: the socket file remains but nothing serves it.
			name: "no server on socket",
			script: func() *exec.Cmd {
				return exec.Command("sh", "-c",
					"echo 'no server running on /tmp/tmux-1000/tclaude' >&2; exit 1")
			},
			wantAlive: false, wantKnown: true,
		},
		{
			name: "transient failure stays unknown",
			script: func() *exec.Cmd {
				return exec.Command("sh", "-c", "echo 'server exited unexpectedly' >&2; exit 1")
			},
			wantAlive: false, wantKnown: false,
		},
		{
			name: "listed session is alive",
			script: func() *exec.Cmd {
				return exec.Command("echo", "tmux-under-test")
			},
			wantAlive: true, wantKnown: true,
		},
		{
			name: "server alive without the session",
			script: func() *exec.Cmd {
				return exec.Command("echo", "some-other-session")
			},
			wantAlive: false, wantKnown: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := clcommon.Default
			clcommon.Default = listSessionsTmux{script: tc.script}
			t.Cleanup(func() { clcommon.Default = prev })
			alive, known := lifecycleSessionAlive("tmux-under-test")
			assert.Equal(t, tc.wantAlive, alive, "alive")
			assert.Equal(t, tc.wantKnown, known, "known")
		})
	}
}

// A wedged server is NOT a dead server: it may still own live panes, so a
// timed-out listing must stay unknown even though the timeout error text
// could in principle carry arbitrary stderr.
func TestTmuxServerNotRunning_TimeoutStaysUnknown(t *testing.T) {
	assert.False(t, tmuxServerNotRunning(
		fmt.Errorf("%w after %s: no server running on /tmp/x", errTmuxCommandTimeout, time.Second)))
	assert.False(t, tmuxServerNotRunning(nil))
}
