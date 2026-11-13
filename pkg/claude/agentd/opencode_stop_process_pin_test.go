package agentd

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxtest"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// These pin stopOpenCodeProcess's three teardown branches by their end state:
// a server this daemon started directly, a server held in a managed tmux
// session, and a recovered PID proven to be ours (killed directly, or through
// its tmux pane). Every branch leaves no registered process. The Linux-only
// pins (an escaped child, the Unix control socket) are in the _linux file.
// The resource-cgroup reap has no seam here; KillResourceCgroupMembers has its
// own test.

// stopPinTmux answers each tmux command through reply and records it.
type stopPinTmux struct {
	sync.Mutex
	calls [][]string
	reply func(args []string) *exec.Cmd
}

func (f *stopPinTmux) Command(args ...string) *exec.Cmd {
	f.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.Unlock()
	if f.reply != nil {
		return f.reply(args)
	}
	return exec.Command("false")
}

// Run and AttachArgv make the exec-shaped fake above a clcommon.Tmux.
func (f *stopPinTmux) Run(ctx context.Context, args []string) ([]byte, error) {
	return tmuxtest.Run(ctx, f.Command(args...))
}

func (f *stopPinTmux) AttachArgv(args []string) ([]string, error) {
	return tmuxtest.Argv(f.Command(args...))
}

func (f *stopPinTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func (f *stopPinTmux) called(verb string) [][]string {
	f.Lock()
	defer f.Unlock()
	var out [][]string
	for _, call := range f.calls {
		if slices.Contains(call, verb) {
			out = append(out, call)
		}
	}
	return out
}

func useStopPinTmux(t *testing.T, fake *stopPinTmux) {
	t.Helper()
	previous := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = previous })
}

// eventuallyDead polls until pid has exited.
func eventuallyDead(t *testing.T, pid int, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for session.IsProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.False(t, session.IsProcessAlive(pid), why)
}

// reapedSleeper is a live process the test owns and reaps as soon as it dies,
// so its exit is observable to IsProcessAlive.
func reapedSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

// killOnCleanup is the safety net for a regressed stop: whatever the test
// started is killed even if the stop under test left it running.
func killOnCleanup(t *testing.T, pids ...int) {
	t.Helper()
	t.Cleanup(func() {
		for _, pid := range pids {
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		}
	})
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 serves the stand-in OpenCode health endpoint")
	}
}

func TestStopOpenCodeProcess_ADirectServerIsStoppedAndDeregistered(t *testing.T) {
	requirePython(t)
	runtime, authority := openCodeProcessPin(t, healthyOpenCodeScript)
	process, err := startOpenCodeProcessWithAuthority(&runtime, nil, authority)
	require.NoError(t, err)
	killOnCleanup(t, runtime.PID)

	stopOpenCodeProcess(runtime, process)

	eventuallyDead(t, runtime.PID, "the server exited")
	assert.Nil(t, registeredOpenCodeProcess(runtime.SessionID), "the process is deregistered")
}

func TestStopOpenCodeProcess_ATmuxHeldServerIsKilledThroughItsSession(t *testing.T) {
	setupTestDB(t)
	fake := &stopPinTmux{reply: func([]string) *exec.Cmd { return exec.Command("true") }}
	useStopPinTmux(t, fake)
	runtime := db.OpenCodeRuntime{SessionID: "spwn-pin-tmux-stop", PID: 99_999_998,
		ServerURL: "http://127.0.0.1:2", Transport: db.OpenCodeTransportLoopbackTCP}
	process := &openCodeProcess{pid: runtime.PID, tmuxSession: "__tclaude-opencode-pin",
		done: make(chan error, 1)}
	process.done <- nil // the session's server has exited
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[runtime.SessionID] = process
	openCodeProcesses.Unlock()

	stopOpenCodeProcess(runtime, nil)

	assert.Equal(t, [][]string{{"-N", "kill-session", "-t", "=__tclaude-opencode-pin"}},
		fake.called("kill-session"), "the registered process's tmux session is killed")
	assert.Nil(t, registeredOpenCodeProcess(runtime.SessionID))
}

func TestStopOpenCodeProcess_AVerifiedRecoveredPIDIsKilled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inPane      bool
		killSession bool
	}{
		{"directly", false, false},
		{"through the tmux pane that holds it", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			pid := reapedSleeper(t)
			fake := &stopPinTmux{reply: func(args []string) *exec.Cmd {
				switch {
				case tc.inPane && slices.Contains(args, "display-message"):
					return exec.Command("echo", strconv.Itoa(pid))
				case slices.Contains(args, "kill-session"):
					return exec.Command("kill", "-9", strconv.Itoa(pid))
				}
				return exec.Command("false")
			}}
			useStopPinTmux(t, fake)
			previous := openCodeRuntimeVerified
			openCodeRuntimeVerified = func(db.OpenCodeRuntime) bool { return true }
			t.Cleanup(func() { openCodeRuntimeVerified = previous })
			runtime := db.OpenCodeRuntime{SessionID: "spwn-pin-recovered", PID: pid,
				ServerURL: "http://127.0.0.1:2", Transport: db.OpenCodeTransportLoopbackTCP}

			stopOpenCodeProcess(runtime, nil)

			eventuallyDead(t, pid, "the verified recovered server was killed")
			if tc.killSession {
				assert.Equal(t, [][]string{{"-N", "kill-session", "-t",
					"=" + openCodeManagedTmuxSession(runtime.SessionID)}},
					fake.called("kill-session"), "the pane's session is killed")
			} else {
				assert.Empty(t, fake.called("kill-session"), "a PID outside tmux is killed directly")
			}
			assert.Nil(t, registeredOpenCodeProcess(runtime.SessionID))
		})
	}
}
