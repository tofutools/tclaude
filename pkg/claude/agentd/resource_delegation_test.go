package agentd

import (
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

type openCodeTmuxProbeFake struct {
	sync.Mutex
	results []bool
	calls   [][]string
}

func (f *openCodeTmuxProbeFake) Command(args ...string) *exec.Cmd {
	f.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	succeed := true
	if len(f.results) > 0 {
		succeed = f.results[0]
		f.results = f.results[1:]
	}
	f.Unlock()
	if succeed {
		return exec.Command("true")
	}
	return exec.Command("false")
}

func (f *openCodeTmuxProbeFake) ListSessions() (map[string]struct{}, error) {
	return nil, nil
}

func TestResolveResourceDelegationDirPrecedence(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "/from-env")
	cfg := &config.Config{Agent: &config.AgentConfig{ResourceDelegationDir: "/from-config"}}

	got, source := resolveResourceDelegationDir("/from-flag", cfg)
	assert.Equal(t, "/from-flag", got)
	assert.Equal(t, "flag", source)

	got, source = resolveResourceDelegationDir("", cfg)
	assert.Equal(t, "/from-env", got)
	assert.Equal(t, "environment", source)

	t.Setenv(session.ResourceDelegationDirEnv, "")
	got, source = resolveResourceDelegationDir("", cfg)
	assert.Equal(t, "/from-config", got)
	assert.Equal(t, "config", source)

	got, source = resolveResourceDelegationDir("", nil)
	assert.Empty(t, got)
	assert.Equal(t, "legacy self-cgroup derivation", source)
}

func TestManagedOpenCodeTmuxSessionNameIsStableAndBounded(t *testing.T) {
	first := openCodeManagedTmuxSession("ses_same")
	assert.Equal(t, first, openCodeManagedTmuxSession("ses_same"))
	assert.NotEqual(t, first, openCodeManagedTmuxSession("ses_other"))
	assert.Regexp(t, `^__tclaude-opencode-[0-9a-f]{20}$`, first)
}

func TestManagedOpenCodeTmuxReclaimsReservedOrphan(t *testing.T) {
	fake := &openCodeTmuxProbeFake{results: []bool{true, true}}
	previous := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = previous })

	require.NoError(t, reclaimOrphanedOpenCodeTmuxSession("__tclaude-opencode-deadbeef"))
	require.Len(t, fake.calls, 2)
	assert.Equal(t, []string{"-N", "has-session", "-t", "=__tclaude-opencode-deadbeef"}, fake.calls[0])
	assert.Equal(t, []string{"-N", "kill-session", "-t", "=__tclaude-opencode-deadbeef"}, fake.calls[1])
}

func TestManagedOpenCodeTmuxWatcherToleratesTransientFailures(t *testing.T) {
	fake := &openCodeTmuxProbeFake{results: []bool{false, false, true, false, false, false}}
	previousTmux := clcommon.Default
	previousInterval := openCodeTmuxProbeInterval
	clcommon.Default = fake
	openCodeTmuxProbeInterval = time.Millisecond
	t.Cleanup(func() {
		clcommon.Default = previousTmux
		openCodeTmuxProbeInterval = previousInterval
	})
	process := &openCodeProcess{pid: 4242, tmuxSession: "__tclaude-opencode-watch",
		done: make(chan error, 1)}

	go watchOpenCodeTmuxProcess(process, db.OpenCodeRuntime{SessionID: "watch-session"})
	select {
	case err := <-process.done:
		require.ErrorContains(t, err, "managed tmux session exited")
	case <-time.After(time.Second):
		t.Fatal("tmux watcher did not reach the consecutive-failure threshold")
	}
	fake.Lock()
	defer fake.Unlock()
	assert.Len(t, fake.calls, 6, "a successful probe must reset the failure count")
}

func TestStopManagedOpenCodeTmuxWaitsForPaneTreeAfterSuccessfulKill(t *testing.T) {
	fake := &openCodeTmuxProbeFake{results: []bool{true}}
	previous := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = previous })
	child := exec.Command("sleep", "10")
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	process := &openCodeProcess{pid: child.Process.Pid, tmuxSession: "__tclaude-opencode-stop",
		done: make(chan error, 1)}

	started := time.Now()
	stopOpenCodeProcess(db.OpenCodeRuntime{SessionID: "stop-session", PID: process.pid}, process)
	assert.GreaterOrEqual(t, time.Since(started), openCodeProcessStopWait,
		"tmux command success must not be treated as pane-tree exit")
	assert.Less(t, time.Since(started), openCodeProcessStopWait+time.Second,
		"pane-tree exit wait must remain bounded")

	select {
	case err := <-process.done:
		require.NoError(t, err)
	default:
		t.Fatal("successful tmux kill did not complete the managed process")
	}
	fake.Lock()
	defer fake.Unlock()
	require.Len(t, fake.calls, 1)
	assert.Equal(t, []string{"-N", "kill-session", "-t", "=__tclaude-opencode-stop"}, fake.calls[0])
}

func TestManagedOpenCodeTmuxUnixHandshakeCrossesProcessBoundary(t *testing.T) {
	handshake, err := prepareOpenCodeTmuxHandshake()
	require.NoError(t, err)
	t.Cleanup(handshake.close)

	childDone := make(chan error, 1)
	go func() {
		status, openErr := os.OpenFile(handshake.statusPath, os.O_WRONLY, 0)
		if openErr != nil {
			childDone <- openErr
			return
		}
		defer status.Close()
		gate, openErr := os.Open(handshake.gatePath)
		if openErr != nil {
			childDone <- openErr
			return
		}
		defer gate.Close()
		if writeErr := opencodeapi.WriteUnixLaunchAuthority(status,
			opencodeapi.UnixLaunchAuthority{Device: 41, Inode: 42}); writeErr != nil {
			childDone <- writeErr
			return
		}
		var acknowledgement [1]byte
		_, readErr := io.ReadFull(gate, acknowledgement[:])
		if readErr == nil && acknowledgement[0] != 1 {
			readErr = io.ErrUnexpectedEOF
		}
		childDone <- readErr
	}()

	require.NoError(t, handshake.connectGate(time.Now().Add(time.Second)))
	authority, err := opencodeapi.ReadUnixLaunchAuthority(handshake.status)
	require.NoError(t, err)
	assert.Equal(t, opencodeapi.UnixLaunchAuthority{Device: 41, Inode: 42}, authority)
	_, err = handshake.gate.Write([]byte{1})
	require.NoError(t, err)
	require.NoError(t, <-childDone)
}

func TestManagedOpenCodeTmuxUnixHandshakeTimesOutAfterGateConnect(t *testing.T) {
	handshake, err := prepareOpenCodeTmuxHandshake()
	require.NoError(t, err)
	t.Cleanup(handshake.close)

	release := make(chan struct{})
	childDone := make(chan struct{})
	go func() {
		defer close(childDone)
		status, openErr := os.OpenFile(handshake.statusPath, os.O_WRONLY, 0)
		if openErr != nil {
			return
		}
		defer status.Close()
		gate, openErr := os.Open(handshake.gatePath)
		if openErr != nil {
			return
		}
		defer gate.Close()
		<-release
	}()

	require.NoError(t, handshake.connectGate(time.Now().Add(time.Second)))
	process := &openCodeProcess{done: make(chan error, 1)}
	_, err = awaitOpenCodeTmuxAuthority(handshake, process, 20*time.Millisecond)
	require.ErrorContains(t, err, "authority handshake timed out")
	close(release)
	<-childDone
}

func TestManagedServerDropsStoredCgroupFromPreviousDelegationBeforeReprepare(t *testing.T) {
	setupTestDB(t)
	t.Setenv("TMUX", "")
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "managed-old-cgroup", ConvID: "ses_old_cgroup",
		ServerURL: "http://127.0.0.1:43210", Cwd: t.TempDir(),
		ResourceCgroupDir: "/sys/fs/cgroup/system.slice/tclaude-agentd.service/tclaude-old",
	}))
	t.Setenv(session.ResourceDelegationDirEnv,
		"/sys/fs/cgroup/system.slice/tclaude-tmux.service")
	previousPrepare := prepareResourceCgroup
	prepareResourceCgroup = func(sessionID string, limits sandboxpolicy.ResourceLimits) (string, func(), error) {
		assert.Equal(t, "managed-old-cgroup", sessionID)
		assert.Equal(t, "128MB", limits.Memory)
		return "/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-new", func() {}, nil
	}
	t.Cleanup(func() { prepareResourceCgroup = previousPrepare })
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "128MB"},
	}}

	dir, _, err := prepareManagedServerResourceCgroup(
		"managed-old-cgroup", snapshot, false)
	require.NoError(t, err)
	assert.Equal(t, "/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-new", dir)
	stored, lookupErr := db.GetOpenCodeRuntime("managed-old-cgroup")
	require.NoError(t, lookupErr)
	assert.Nil(t, stored, "the invalid old-root runtime must be stopped before a fresh cgroup is prepared")
}
