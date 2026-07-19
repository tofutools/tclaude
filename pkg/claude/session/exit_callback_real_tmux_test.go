package session

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
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

const (
	realTmuxPaneHelperEnv    = "TCLAUDE_REAL_TMUX_PANE_HELPER"
	realTmuxPaneHelperMarker = "tclaude-real-tmux-pane-helper"
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

// skipTmux34PaneDeathEvidenceLoss gates the real-tmux proofs that depend on
// tmux delivering dead-pane evidence. tmux 3.4 built WITH systemd support —
// how Ubuntu ships it, and therefore what ubuntu-latest CI runs — can mark a
// pane dead while recording neither pane_dead_status nor pane_dead_signal and
// never firing the pane-local pane-died hook. The loss is nondeterministic
// (roughly half of runs against tmux_3.4-1ubuntu0.1; hooks verified installed,
// evidence probe returns "%N|1||"). Vanilla tmux 3.4 built from the upstream
// release without libsystemd delivers the same contract reliably (10/10 local
// runs), so the skip probes the binary's systemd linkage instead of blanket
// version matching; a failed probe conservatively counts as affected. This is
// a tmux-side defect in one distribution build, not something a fixture can
// compensate for — production already degrades to the daemon reaper when the
// hook never fires.
func skipTmux34PaneDeathEvidenceLoss(t *testing.T) {
	t.Helper()
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil || strings.TrimSpace(string(out)) != "tmux 3.4" {
		return
	}
	path, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux 3.4: cannot resolve binary; assuming systemd-linked pane-death evidence loss")
	}
	ldd, err := exec.Command("ldd", path).Output()
	if err != nil || strings.Contains(string(ldd), "libsystemd") {
		t.Skip("tmux 3.4 with systemd support nondeterministically loses dead-pane evidence (no pane-died event, empty pane_dead_status)")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
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

func waitForPaneOption(t *testing.T, tmux isolatedRealTmux, paneID, option, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastValue string
	var lastErr error
	for time.Now().Before(deadline) {
		out, err := tmux.Command("show-options", "-p", "-v", "-t", paneID, option).Output()
		if err == nil {
			lastValue = strings.TrimSpace(string(out))
			if lastValue == want {
				return
			}
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	version, versionErr := tmux.Command("-V").CombinedOutput()
	hook, hookErr := tmux.Command("show-hooks", "-p", "-t", paneID, "pane-died").CombinedOutput()
	evidence, evidenceErr := tmux.Command("display-message", "-p", "-t", paneID,
		"#{pane_id}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}").CombinedOutput()
	t.Fatalf("timed out waiting for pane-died pane option %s=%s on %s: last_value=%q last_error=%v tmux=%q tmux_error=%v hook=%q hook_error=%v evidence=%q evidence_error=%v",
		option, want, paneID, lastValue, lastErr, strings.TrimSpace(string(version)), versionErr,
		strings.TrimSpace(string(hook)), hookErr, strings.TrimSpace(string(evidence)), evidenceErr)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return pid
			}
			lastErr = parseErr
		} else {
			lastErr = err
		}
		if raw, err := os.ReadFile(path + ".error"); err == nil {
			t.Fatalf("exact helper pid unavailable: %s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for exact helper pid in %s: %v", path, lastErr)
	return 0
}

func realTmuxPaneHelperCommand(t *testing.T, readyPath, releasePath, errorPath string, code int) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	args := []string{
		"env", realTmuxPaneHelperEnv + "=1", exe,
		"-test.run=^TestRealTmuxPaneProcessHelper$", "--",
		realTmuxPaneHelperMarker, readyPath, releasePath, errorPath, strconv.Itoa(code),
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, clcommon.ShellQuoteArg(arg))
	}
	return strings.Join(quoted, " ")
}

func TestRealTmuxPaneProcessHelper(t *testing.T) {
	if os.Getenv(realTmuxPaneHelperEnv) != "1" {
		return
	}
	args := flag.Args()
	if len(args) != 5 || args[0] != realTmuxPaneHelperMarker {
		t.Fatalf("invalid real tmux pane helper arguments")
	}
	readyPath, releasePath, errorPath := args[1], args[2], args[3]
	code, err := strconv.Atoi(args[4])
	if err != nil || code < 0 || code > 255 {
		_ = os.WriteFile(errorPath, []byte("invalid helper exit code"), 0o600)
		os.Exit(126)
	}
	if err := os.WriteFile(readyPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.WriteFile(errorPath, []byte("write helper ready marker: "+err.Error()), 0o600)
		os.Exit(126)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(releasePath); err == nil {
			os.Exit(code)
		} else if !os.IsNotExist(err) {
			_ = os.WriteFile(errorPath, []byte("inspect helper release marker: "+err.Error()), 0o600)
			os.Exit(126)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = os.WriteFile(errorPath, []byte("timed out waiting for helper release or signal"), 0o600)
	os.Exit(124)
}

func TestRealTmuxPaneDiedEmitsAndPreservesTruthfulBootstrapEvidence(t *testing.T) {
	skipTmux34PaneDeathEvidenceLoss(t)
	tmux := withIsolatedRealTmux(t)
	tests := []struct {
		name        string
		release     bool
		signalPane  syscall.Signal
		helperCode  int
		wantCode    *int
		wantSignals []string
	}{
		{name: "exit-0", release: true, helperCode: 0, wantCode: intPtr(0), wantSignals: []string{""}},
		{name: "exit-9", release: true, helperCode: 9, wantCode: intPtr(9), wantSignals: []string{""}},
		{name: "sigterm", signalPane: syscall.SIGTERM, wantSignals: []string{"15", "TERM"}},
		{name: "sigkill", signalPane: syscall.SIGKILL, wantSignals: []string{"9", "KILL"}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			release := filepath.Join(dir, "release")
			ready := filepath.Join(dir, "ready")
			helperError := ready + ".error"
			name := fmt.Sprintf("tcl573-%d", i)
			paneCommand := "exec " + realTmuxPaneHelperCommand(
				t, ready, release, helperError, tc.helperCode)
			require.NoError(t, tmux.Command("new-session", "-d", "-s", name, paneCommand).Run())
			requireNativePaneDied(t, tmux)
			target := name + ":0.0"
			require.NoError(t, tmux.Command("set-option", "-p", "-t", target, "remain-on-exit", "on").Run())
			generation := fmt.Sprintf("%032x", i+1)
			require.NoError(t, tmux.Command("set-option", "-p", "-t", target,
				paneExitGenerationOption, generation).Run())
			paneRaw, err := tmux.Command("display-message", "-p", "-t", target, "#{pane_id}").Output()
			require.NoError(t, err)
			paneID := strings.TrimSpace(string(paneRaw))
			require.True(t, validCallbackPaneID(paneID))
			const emittedOption = "@tcl573_test_pane_died_emitted"
			emittedToken := fmt.Sprintf("event-%d", i+1)
			hook := "set-option -p -t " + paneID + " " + emittedOption + " " + emittedToken
			require.NoError(t, tmux.Command("set-hook", "-p", "-t", target, "pane-died", hook).Run())
			helperPID := waitForPIDFile(t, ready)
			out, err := tmux.Command("display-message", "-p", "-t", target, "#{pane_pid}").Output()
			require.NoError(t, err)
			panePID, err := strconv.Atoi(strings.TrimSpace(string(out)))
			require.NoError(t, err)
			require.Equal(t, helperPID, panePID, "pane PID must be the ready helper itself")
			if tc.release {
				require.NoError(t, os.WriteFile(release, []byte("go"), 0o600))
			} else {
				require.NoError(t, syscall.Kill(panePID, tc.signalPane))
			}
			waitForPaneOption(t, tmux, paneID, emittedOption, emittedToken)
			evidence, err := InspectDeadTmuxSessionPane(name)
			require.NoError(t, err)
			if tc.wantCode == nil {
				assert.Nil(t, evidence.ExitCode)
			} else {
				require.NotNil(t, evidence.ExitCode)
				assert.Equal(t, *tc.wantCode, *evidence.ExitCode)
			}
			assert.Contains(t, tc.wantSignals, evidence.Signal,
				"tmux may report signals by number or name depending on platform support")
			assert.Equal(t, generation, evidence.Generation)
			require.NoError(t, CleanupDeadTmuxPane(evidence))
		})
	}
}

func TestRealTmuxAuthenticatedPaneDiedCallbackRecordsExactlyOnce(t *testing.T) {
	// TCL-573 left this proof enabled on tmux 3.4, but it depends on the same
	// pane-died delivery the systemd-linked 3.4 builds lose nondeterministically
	// (reproduced: the marker never appears because the hook never fires), so it
	// was a latent CI flake on ubuntu-latest. Same gate, same reason.
	skipTmux34PaneDeathEvidenceLoss(t)
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
	skipTmux34PaneDeathEvidenceLoss(t)
	tmux := withIsolatedRealTmux(t)
	for _, tc := range []struct {
		signal syscall.Signal
		code   int
	}{{syscall.SIGTERM, 143}, {syscall.SIGKILL, 137}} {
		name := fmt.Sprintf("tcl573-child-%d", tc.code)
		dir := t.TempDir()
		script := filepath.Join(dir, "launch-wrapper.sh")
		ready := filepath.Join(dir, "helper-ready")
		release := filepath.Join(dir, "never-release")
		childPIDPath := filepath.Join(dir, "helper.pid")
		helperCommand := realTmuxPaneHelperCommand(t, ready, release, ready+".error", 0)
		require.NoError(t, os.WriteFile(script,
			[]byte("#!/bin/sh\n"+helperCommand+" &\nchild=$!\nprintf '%s' \"$child\" > "+
				clcommon.ShellQuoteArg(childPIDPath)+"\nwait \"$child\"\nstatus=$?\nexit \"$status\"\n"), 0o700))
		paneCommand := "exec /bin/sh " + clcommon.ShellQuoteArg(script)
		require.NoError(t, tmux.Command("new-session", "-d", "-s", name, paneCommand).Run())
		requireNativePaneDied(t, tmux)
		target := name + ":0.0"
		require.NoError(t, tmux.Command("set-option", "-p", "-t", target, "remain-on-exit", "on").Run())
		generation := fmt.Sprintf("%032x", tc.code)
		require.NoError(t, tmux.Command("set-option", "-p", "-t", target,
			paneExitGenerationOption, generation).Run())
		helperPID := waitForPIDFile(t, ready)
		childPID := waitForPIDFile(t, childPIDPath)
		require.Equal(t, helperPID, childPID, "wrapper $! must be the ready helper itself")
		require.NoError(t, syscall.Kill(childPID, tc.signal))
		deadline := time.Now().Add(10 * time.Second)
		var evidence PaneExitEvidence
		var err error
		for time.Now().Before(deadline) {
			evidence, err = InspectDeadTmuxSessionPane(name)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		require.NoError(t, err)
		require.NotNil(t, evidence.ExitCode, "dead pane evidence: %#v", evidence)
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
