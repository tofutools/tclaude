package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type isolatedRealTmux struct{ socket string }

func (t isolatedRealTmux) Command(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-S", t.socket}, args...)...)
}

func (t isolatedRealTmux) ListSessions() (map[string]struct{}, error) {
	out, err := t.Command("list-sessions", "-F", "#{session_name}\t#{pane_dead}").Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	alive := map[string]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if fields[0] != "" && (len(fields) == 1 || fields[1] != "1") {
			alive[fields[0]] = struct{}{}
		}
	}
	return alive, nil
}

func withIsolatedRealTmux(t *testing.T) isolatedRealTmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("tcl573-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	tmux := isolatedRealTmux{socket: socket}
	if err := tmux.Command("new-session", "-d", "-s", "tcl573-keepalive", "sleep", "300").Run(); err != nil {
		t.Fatalf("start isolated tmux keepalive: %v", err)
	}
	previous := clcommon.Default
	clcommon.Default = tmux
	t.Cleanup(func() {
		_ = tmux.Command("kill-server").Run()
		_ = os.Remove(socket)
		clcommon.Default = previous
	})
	return tmux
}

func requireNativePaneDied(t *testing.T, tmux isolatedRealTmux) {
	t.Helper()
	out, err := tmux.Command("show-hooks", "-g", "pane-died").Output()
	if err != nil || !strings.Contains(string(out), "pane-died") {
		t.Skip("tmux lacks native pane-died hook")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if raw, err := os.ReadFile(path + ".error"); err == nil {
			t.Fatalf("pane-died callback failed: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pane-died marker %s", path)
}

func waitForChildPID(t *testing.T, parent int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("pgrep", "-P", strconv.Itoa(parent)).Output()
		if err == nil {
			line := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
			if pid, parseErr := strconv.Atoi(line); parseErr == nil && pid > 0 {
				comm, commErr := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
				if commErr == nil && strings.TrimSpace(string(comm)) == "sleep" {
					return pid
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for child of pane pid %d", parent)
	return 0
}

func TestRealTmuxPaneDiedEmitsAndPreservesTruthfulBootstrapEvidence(t *testing.T) {
	tmux := withIsolatedRealTmux(t)
	tests := []struct {
		name       string
		command    string
		release    bool
		signalPane syscall.Signal
		wantCode   *int
		wantSignal string
	}{
		{name: "exit-0", command: "while [ ! -f RELEASE ]; do sleep 0.01; done; exit 0", release: true, wantCode: intPtr(0)},
		{name: "exit-9", command: "while [ ! -f RELEASE ]; do sleep 0.01; done; exit 9", release: true, wantCode: intPtr(9)},
		{name: "sigterm", command: "exec sleep 30", signalPane: syscall.SIGTERM, wantSignal: "15"},
		{name: "sigkill", command: "exec sleep 30", signalPane: syscall.SIGKILL, wantSignal: "9"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			release := filepath.Join(dir, "release")
			marker := filepath.Join(dir, "emitted")
			command := strings.ReplaceAll(tc.command, "RELEASE", clcommon.ShellQuoteArg(release))
			name := fmt.Sprintf("tcl573-%d", i)
			require.NoError(t, tmux.Command("new-session", "-d", "-s", name,
				"/bin/sh", "-c", command).Run())
			requireNativePaneDied(t, tmux)
			target := name + ":0.0"
			require.NoError(t, tmux.Command("set-option", "-p", "-t", target, "remain-on-exit", "on").Run())
			generation := fmt.Sprintf("%032x", i+1)
			require.NoError(t, tmux.Command("set-option", "-p", "-t", target,
				paneExitGenerationOption, generation).Run())
			hook := "run-shell " + clcommon.ShellQuoteArg("printf emitted > "+clcommon.ShellQuoteArg(marker))
			require.NoError(t, tmux.Command("set-hook", "-p", "-t", target, "pane-died", hook).Run())
			if tc.release {
				require.NoError(t, os.WriteFile(release, []byte("go"), 0o600))
			} else {
				out, err := tmux.Command("display-message", "-p", "-t", target, "#{pane_pid}").Output()
				require.NoError(t, err)
				pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
				require.NoError(t, err)
				pgid, err := syscall.Getpgid(pid)
				require.NoError(t, err)
				require.NotEqual(t, syscall.Getpgrp(), pgid, "pane process group must be isolated from the test runner")
				require.NoError(t, syscall.Kill(-pgid, tc.signalPane))
			}
			waitForFile(t, marker)
			evidence, err := InspectDeadTmuxSessionPane(name)
			require.NoError(t, err)
			if tc.wantCode == nil {
				assert.Nil(t, evidence.ExitCode)
			} else {
				require.NotNil(t, evidence.ExitCode)
				assert.Equal(t, *tc.wantCode, *evidence.ExitCode)
			}
			assert.Equal(t, tc.wantSignal, evidence.Signal)
			assert.Equal(t, generation, evidence.Generation)
			require.NoError(t, CleanupDeadTmuxPane(evidence))
		})
	}
}

func TestRealTmuxAuthenticatedPaneDiedCallbackRecordsExactlyOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	tmux := withIsolatedRealTmux(t)
	db.ResetForTest()
	const sessionID = "spwn-real-callback"
	const tmuxName = "tcl573-real-callback"
	const generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const token = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: sessionID, TmuxSession: tmuxName, ConvID: "conv-real-callback",
		Status: StatusWorking, Created: time.Now(),
	}, generation, db.SessionExitGatePending))

	release := filepath.Join(t.TempDir(), "release")
	command := "while [ ! -f " + clcommon.ShellQuoteArg(release) + " ]; do sleep 0.01; done; exit 9"
	require.NoError(t, tmux.Command("new-session", "-d", "-s", tmuxName,
		"/bin/sh -c "+clcommon.ShellQuoteArg(command)).Run())
	requireNativePaneDied(t, tmux)
	target := tmuxName + ":0.0"
	require.NoError(t, tmux.Command("set-option", "-p", "-t", target, "remain-on-exit", "on").Run())
	require.NoError(t, tmux.Command("set-option", "-p", "-t", target,
		paneExitGenerationOption, generation).Run())
	paneRaw, err := tmux.Command("display-message", "-p", "-t", target, "#{pane_id}").Output()
	require.NoError(t, err)
	paneID := strings.TrimSpace(string(paneRaw))
	require.True(t, validCallbackPaneID(paneID))
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		sessionID, generation, hex.EncodeToString(hash[:]), paneID))
	require.NoError(t, db.MarkSessionExitLaunchReleasing(sessionID, generation))
	require.NoError(t, db.MarkSessionExitLaunchReleased(sessionID, generation))

	marker := filepath.Join(t.TempDir(), "authenticated-callback")
	helperArgs := []string{
		"env",
		clcommon.ShellQuoteArg("TCL573_CALLBACK_HELPER=1"),
		clcommon.ShellQuoteArg("TCL573_SOCKET=" + tmux.socket),
		clcommon.ShellQuoteArg("TCL573_MARKER=" + marker),
		clcommon.ShellQuoteArg("TCL573_ERROR=" + marker + ".error"),
		clcommon.ShellQuoteArg("TCL573_SESSION_ID=" + sessionID),
		clcommon.ShellQuoteArg("TCL573_TMUX_SESSION=" + tmuxName),
		clcommon.ShellQuoteArg("TCL573_PANE_ID=" + paneID),
		clcommon.ShellQuoteArg("TCL573_GENERATION=" + generation),
		clcommon.ShellQuoteArg("TCL573_TOKEN=" + token),
		clcommon.ShellQuoteArg("TCL573_EXIT_CODE=#{pane_dead_status}"),
		clcommon.ShellQuoteArg("TCL573_SIGNAL=#{pane_dead_signal}"),
		clcommon.ShellQuoteArg(os.Args[0]),
		"-test.run=^TestRealTmuxAuthenticatedCallbackHelper$",
		"-test.count=1",
	}
	hook := "run-shell " + clcommon.ShellQuoteArg(strings.Join(helperArgs, " "))
	require.NoError(t, tmux.Command("set-hook", "-p", "-t", target, "pane-died", hook).Run())
	require.NoError(t, os.WriteFile(release, []byte("go"), 0o600))
	waitForFile(t, marker)

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.AgentExitObserverTmux, rows[0].Observer)
	assert.Equal(t, db.AgentExitObservedProcessPaneBootstrap, rows[0].ObservedProcess)
	require.NotNil(t, rows[0].ExitCode)
	assert.Equal(t, 9, *rows[0].ExitCode)
	assert.Empty(t, rows[0].Signal)
}

func TestRealTmuxAuthenticatedCallbackHelper(t *testing.T) {
	if os.Getenv("TCL573_CALLBACK_HELPER") != "1" {
		return
	}
	db.ResetForTest()
	clcommon.Default = isolatedRealTmux{socket: os.Getenv("TCL573_SOCKET")}
	err := runExitCallback(exitCallbackParams{
		SessionID:   os.Getenv("TCL573_SESSION_ID"),
		TmuxSession: os.Getenv("TCL573_TMUX_SESSION"),
		PaneID:      os.Getenv("TCL573_PANE_ID"),
		Generation:  os.Getenv("TCL573_GENERATION"),
		Token:       os.Getenv("TCL573_TOKEN"),
		ExitCode:    os.Getenv("TCL573_EXIT_CODE"),
		Signal:      os.Getenv("TCL573_SIGNAL"),
	})
	if err != nil {
		_ = os.WriteFile(os.Getenv("TCL573_ERROR"), []byte(err.Error()), 0o600)
	}
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv("TCL573_MARKER"), []byte("ok"), 0o600))
}

func TestRealTmuxLaunchWrapperChildSignalsAreExitCodesNotPaneSignals(t *testing.T) {
	tmux := withIsolatedRealTmux(t)
	for _, tc := range []struct {
		signal syscall.Signal
		code   int
	}{{syscall.SIGTERM, 143}, {syscall.SIGKILL, 137}} {
		name := fmt.Sprintf("tcl573-child-%d", tc.code)
		script := filepath.Join(t.TempDir(), "launch-wrapper.sh")
		require.NoError(t, os.WriteFile(script,
			[]byte("#!/bin/sh\nsleep 30 &\nchild=$!\nwait \"$child\"\nstatus=$?\nexit $status\n"), 0o700))
		paneCommand := "exec /bin/sh " + clcommon.ShellQuoteArg(script)
		require.NoError(t, tmux.Command("new-session", "-d", "-s", name, paneCommand).Run())
		requireNativePaneDied(t, tmux)
		target := name + ":0.0"
		require.NoError(t, tmux.Command("set-option", "-p", "-t", target, "remain-on-exit", "on").Run())
		generation := fmt.Sprintf("%032x", tc.code)
		require.NoError(t, tmux.Command("set-option", "-p", "-t", target,
			paneExitGenerationOption, generation).Run())
		out, err := tmux.Command("display-message", "-p", "-t", target, "#{pane_pid}").Output()
		require.NoError(t, err)
		panePID, err := strconv.Atoi(strings.TrimSpace(string(out)))
		require.NoError(t, err)
		childPID := waitForChildPID(t, panePID)
		require.NoError(t, syscall.Kill(childPID, tc.signal))
		deadline := time.Now().Add(3 * time.Second)
		var evidence PaneExitEvidence
		for time.Now().Before(deadline) {
			evidence, err = InspectDeadTmuxSessionPane(name)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		require.NoError(t, err)
		require.NotNil(t, evidence.ExitCode)
		assert.Equal(t, tc.code, *evidence.ExitCode)
		assert.Empty(t, evidence.Signal, "child signal must not be attributed as direct pane-bootstrap signal")
		assert.Equal(t, generation, evidence.Generation)
		require.NoError(t, CleanupDeadTmuxPane(evidence))
	}
}

func TestRealTmuxKillPaneDestroysEvidenceWithoutPaneDiedEvent(t *testing.T) {
	tmux := withIsolatedRealTmux(t)
	marker := filepath.Join(t.TempDir(), "emitted")
	require.NoError(t, tmux.Command("new-session", "-d", "-s", "tcl573-kill-pane", "exec sleep 30").Run())
	requireNativePaneDied(t, tmux)
	target := "tcl573-kill-pane:0.0"
	hook := "run-shell " + clcommon.ShellQuoteArg("printf emitted > "+clcommon.ShellQuoteArg(marker))
	require.NoError(t, tmux.Command("set-hook", "-p", "-t", target, "pane-died", hook).Run())
	require.NoError(t, tmux.Command("kill-pane", "-t", target).Run())
	time.Sleep(100 * time.Millisecond)
	_, err := os.Stat(marker)
	assert.ErrorIs(t, err, os.ErrNotExist, "explicit kill-pane does not emit pane-died")
	_, err = InspectDeadTmuxSessionPane("tcl573-kill-pane")
	assert.Error(t, err, "destroyed pane has no status/signal evidence to infer")
}

func intPtr(v int) *int { return &v }
