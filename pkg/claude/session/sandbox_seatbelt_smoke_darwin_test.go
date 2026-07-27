//go:build darwin

package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	darwinSmokeHelperEnv            = "TCLAUDE_SANDBOX_V2_DARWIN_HELPER"
	darwinSmokeAllowedEnv           = "TCLAUDE_SANDBOX_V2_ALLOWED"
	darwinSmokeOutsideEnv           = "TCLAUDE_SANDBOX_V2_OUTSIDE"
	darwinSmokeReadonlyEnv          = "TCLAUDE_SANDBOX_V2_READONLY"
	darwinSmokeHiddenEnv            = "TCLAUDE_SANDBOX_V2_HIDDEN"
	darwinSmokeAliasFileEnv         = "TCLAUDE_SANDBOX_V2_ALIAS_FILE"
	darwinSmokeProtectedFileEnv     = "TCLAUDE_SANDBOX_V2_PROTECTED_FILE"
	darwinSmokePolicySocketEnv      = "TCLAUDE_SANDBOX_V2_POLICY_SOCKET"
	darwinSmokeTmuxSocketEnv        = "TCLAUDE_SANDBOX_V2_TMUX_SOCKET"
	darwinSmokeTclaudeBinaryEnv     = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	darwinSmokeRestrictBaselineEnv  = "TCLAUDE_SANDBOX_V2_RESTRICT_BASELINE"
	darwinSmokeExerciseBrokerEnv    = "TCLAUDE_SANDBOX_V2_EXERCISE_BROKER"
	darwinSmokeNetworkIsolatedEnv   = "TCLAUDE_SANDBOX_V2_NETWORK_ISOLATED"
	darwinSmokeHostListenerEnv      = "TCLAUDE_SANDBOX_V2_HOST_LISTENER"
	darwinSmokeExpectedAgentIDEnv   = "TCLAUDE_SANDBOX_V2_EXPECTED_AGENT_ID"
	darwinSmokeRuntimeTempDirEnv    = "TCLAUDE_SANDBOX_V2_RUNTIME_TMPDIR"
	darwinSmokeInheritedFDEnv       = "TCLAUDE_SANDBOX_V2_INHERITED_FD"
	darwinSmokeHelperPIDFileEnv     = "TCLAUDE_SANDBOX_V2_HELPER_PID_FILE"
	darwinSmokeHelperGateFDEnv      = "TCLAUDE_SANDBOX_V2_HELPER_GATE_FD"
	darwinSmokeHelperTestExpression = "^TestTclaudeLayerDarwinSmokeHelper$"
)

const darwinSmokeConvID = "77000000-0000-4000-8000-000000000770"
const darwinSmokeSessionID = "sandbox-v2-darwin-smoke"

func TestStackedSandboxDarwinRefusal(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on macOS")
	}
	for _, name := range []string{harness.DefaultName, harness.CodexName} {
		_, err := StackedSandboxAvailability(harness.MustGet(name))
		require.ErrorContains(t, err, "stacked requested — refused")
		require.ErrorContains(t, err, "missing capability stacked_nested_seatbelt")
		require.ErrorContains(t, err, "refusing rather than falling back")
	}
}

func TestTclaudeLayerDarwinSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on macOS to exercise sandbox-exec")
	}
	binary, _, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(home, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-seatbelt-smoke-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)

	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	readonly := filepath.Join(root, "readonly")
	hidden := filepath.Join(readonly, "hidden")
	realTools := filepath.Join(root, "real-tools")
	aliasTools := filepath.Join(root, "alias-tools")
	smokeHome := filepath.Join(root, "home")
	protectedDir := filepath.Join(smokeHome, ".tclaude", "data")
	protectedFile := filepath.Join(protectedDir, "private")
	tmuxBase := filepath.Join(root, "tmux-base")
	for _, dir := range []string{
		allowed,
		outside,
		readonly,
		hidden,
		realTools,
		filepath.Join(smokeHome, ".tclaude", "api"),
		filepath.Join(smokeHome, ".claude"),
		filepath.Join(smokeHome, ".claude", "sessions"),
		protectedDir,
		tmuxBase,
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(outside, "host-file"), []byte("outside"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(readonly, "host-file"), []byte("readonly"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(hidden, "private"), []byte("hidden"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realTools, "probe"), []byte("alias-ok"), 0o600))
	require.NoError(t, os.WriteFile(protectedFile, []byte("protected"), 0o600))
	require.NoError(t, os.Symlink(realTools, aliasTools))
	policySocket := filepath.Join(hidden, "policy.sock")
	policyListener, err := net.Listen("unix", policySocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = policyListener.Close() })
	hostListener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hostListener.Close() })

	t.Setenv("HOME", smokeHome)
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	tclaudeBinary := strings.TrimSpace(os.Getenv(darwinSmokeTclaudeBinaryEnv))
	require.NotEmpty(t, tclaudeBinary,
		"set TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY to the current built tclaude binary")
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	agentSocket := filepath.Join(smokeHome, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startDarwinSeatbeltSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)
	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    darwinSmokeSessionID,
		PID:                   os.Getpid(),
		ConvID:                darwinSmokeConvID,
		Harness:               harness.DefaultName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		Cwd:                   allowed,
		Status:                StatusIdle,
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	expectedAgentID, _, err := db.EnsureAgentForConv(darwinSmokeConvID, "seatbelt-smoke")
	require.NoError(t, err)

	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmuxSocketDir, 0o700))
	tmuxSocket := filepath.Join(tmuxSocketDir, "tclaude")
	tmuxListener, err := net.Listen("unix", tmuxSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tmuxListener.Close() })

	helperBinary := filepath.Join(allowed, "claude")
	copyDarwinSmokeBinary(t, os.Args[0], helperBinary)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Explicit: &sandboxpolicy.Profile{
			Name: "tclaude-seatbelt-smoke",
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: root, Access: sandboxpolicy.AccessDeny},
				{Path: allowed, Access: sandboxpolicy.AccessWrite},
				{Path: readonly, Access: sandboxpolicy.AccessRead},
				{Path: hidden, Access: sandboxpolicy.AccessDeny},
				{Path: aliasTools, Access: sandboxpolicy.AccessRead},
				{Path: tmuxBase, Access: sandboxpolicy.AccessWrite},
			},
		},
	})
	require.NoError(t, err)
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Aliases, sandboxpolicy.MountAlias{Link: aliasTools, Target: realTools})

	phase0, err := tclaudeLayerPhase0WriteDirs(TclaudeLayerLaunchContract{
		HarnessName: harness.DefaultName,
		WriteDirs:   []string{allowed},
	}, effective)
	require.NoError(t, err)
	runtimeTempDir, err := darwinSeatbeltRuntimeTempDir()
	require.NoError(t, err)
	runDarwinSeatbeltSmokeHelper(
		t,
		binary,
		helperBinary,
		phase0,
		plan,
		false,
		true,
		false,
		allowed,
		outside,
		readonly,
		hidden,
		filepath.Join(aliasTools, "probe"),
		protectedFile,
		policySocket,
		tmuxSocket,
		runtimeTempDir,
		tclaudeBinary,
		hostListener.Addr().String(),
		expectedAgentID,
	)

	state, err := LoadSessionState(darwinSmokeSessionID)
	require.NoError(t, err)
	assert.Equal(t, StatusWorking, state.Status,
		"the brokered hook must update the host session row")
	assert.False(t, state.LastHook.IsZero(),
		"the brokered hook must stamp the host row instead of a phantom database")
	snapshot, err := db.GetContextSnapshot(darwinSmokeSessionID)
	require.NoError(t, err)
	assert.Equal(t, "Opus 5", snapshot.Model,
		"the brokered status line must update the host context snapshot")
	assert.Equal(t, "claude-opus-5", snapshot.ModelID)

	isolatedPlan := plan
	isolatedPlan.NetworkPosture = sandboxpolicy.NetworkIsolatedWithAgentd
	runDarwinSeatbeltSmokeHelper(
		t,
		binary,
		helperBinary,
		phase0,
		isolatedPlan,
		false,
		true,
		true,
		allowed,
		outside,
		readonly,
		hidden,
		filepath.Join(aliasTools, "probe"),
		protectedFile,
		policySocket,
		tmuxSocket,
		runtimeTempDir,
		tclaudeBinary,
		hostListener.Addr().String(),
		expectedAgentID,
	)
	isolatedState, err := LoadSessionState(darwinSmokeSessionID)
	require.NoError(t, err)
	assert.True(t, isolatedState.LastHook.After(state.LastHook),
		"the isolated brokered hook must advance the host session row")
	isolatedSnapshot, err := db.GetContextSnapshot(darwinSmokeSessionID)
	require.NoError(t, err)
	assert.Equal(t, "Opus 5 isolated", isolatedSnapshot.Model,
		"the isolated brokered status write must be readable from the host database")

	// The compatibility paths pierce only the baseline deny. Adding explicit
	// RO plan entries for /dev and TMPDIR must make the same writes fail.
	devPolicyPath := darwinSmokeIdentityEquivalentSpelling(t, "/dev", "/DEV")
	runtimePolicyPath := darwinSmokeIdentityEquivalentSpelling(
		t,
		runtimeTempDir,
		strings.Replace(runtimeTempDir, "/private/", "/PRIVATE/", 1),
	)
	restrictedPlan := plan
	restrictedPlan.Entries = append(append([]sandboxpolicy.MountEntry(nil), plan.Entries...),
		sandboxpolicy.MountEntry{Path: devPolicyPath, Mode: sandboxpolicy.MountRO},
		sandboxpolicy.MountEntry{Path: runtimePolicyPath, Mode: sandboxpolicy.MountRO},
	)
	runDarwinSeatbeltSmokeHelper(
		t,
		binary,
		helperBinary,
		phase0,
		restrictedPlan,
		true,
		false,
		false,
		allowed,
		outside,
		readonly,
		hidden,
		filepath.Join(aliasTools, "probe"),
		protectedFile,
		policySocket,
		tmuxSocket,
		runtimeTempDir,
		tclaudeBinary,
		hostListener.Addr().String(),
		expectedAgentID,
	)
}

func runDarwinSeatbeltSmokeHelper(
	t *testing.T,
	binary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	restrictBaseline, exerciseBroker, networkIsolated bool,
	allowed, outside, readonly, hidden, aliasFile, protectedFile, policySocket, tmuxSocket,
	runtimeTempDir, tclaudeBinary, hostListener, expectedAgentID string,
) {
	t.Helper()
	helperCommand := clcommon.ShellQuoteArg(helperBinary) +
		" " + clcommon.ShellQuoteArg("-test.run="+darwinSmokeHelperTestExpression)
	command, err := tclaudeLayerCommand(
		binary,
		phase0WriteDirs,
		nil,
		plan,
		helperCommand,
	)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	inherited, err := os.OpenFile(
		filepath.Join(outside, "fd-carveout"),
		os.O_CREATE|os.O_RDWR|os.O_APPEND,
		0o600,
	)
	require.NoError(t, err)
	defer func() { _ = inherited.Close() }()
	gateReader, gateWriter, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = gateReader.Close() }()
	defer func() { _ = gateWriter.Close() }()
	helperPIDFile := filepath.Join(allowed, ".tclaude-seatbelt-helper.pid")
	_ = os.Remove(helperPIDFile)
	defer func() { _ = os.Remove(helperPIDFile) }()
	cmd.ExtraFiles = []*os.File{inherited, gateReader}
	cmd.Env = append(os.Environ(),
		darwinSmokeHelperEnv+"=1",
		darwinSmokeAllowedEnv+"="+allowed,
		darwinSmokeOutsideEnv+"="+outside,
		darwinSmokeReadonlyEnv+"="+readonly,
		darwinSmokeHiddenEnv+"="+hidden,
		darwinSmokeAliasFileEnv+"="+aliasFile,
		darwinSmokeProtectedFileEnv+"="+protectedFile,
		darwinSmokePolicySocketEnv+"="+policySocket,
		darwinSmokeTmuxSocketEnv+"="+tmuxSocket,
		darwinSmokeTclaudeBinaryEnv+"="+tclaudeBinary,
		darwinSmokeRestrictBaselineEnv+"="+boolString(restrictBaseline),
		darwinSmokeExerciseBrokerEnv+"="+boolString(exerciseBroker),
		darwinSmokeNetworkIsolatedEnv+"="+boolString(networkIsolated),
		darwinSmokeHostListenerEnv+"="+hostListener,
		darwinSmokeExpectedAgentIDEnv+"="+expectedAgentID,
		darwinSmokeRuntimeTempDirEnv+"="+runtimeTempDir,
		darwinSmokeInheritedFDEnv+"=3",
		darwinSmokeHelperPIDFileEnv+"="+helperPIDFile,
		darwinSmokeHelperGateFDEnv+"=4",
		HookBrokerEnvVar+"="+HookBrokerAgentd,
		"TCLAUDE_SESSION_ID="+darwinSmokeSessionID,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	waited := false
	defer func() {
		if waited {
			return
		}
		// Unblock the helper before killing/waiting for the outer shell. This
		// runs on FailNow too, so a setup assertion cannot strand a child or
		// leave os/exec's output-copy goroutines unjoined.
		_ = gateWriter.Close()
		cancel()
		_ = cmd.Wait()
	}()
	require.NoError(t, gateReader.Close())

	// This copied test binary stands in for the real harness process. Register
	// its exact live PID before letting it call agentd: production session
	// launches record the harness/pane PID, while keying this synthetic row to
	// the outer go-test PID makes Darwin's ps-based ancestry walk depend on
	// incidental sandbox-exec shell hops. The gate keeps the identity proof
	// deterministic and, crucially, makes the isolated whoami assertion test
	// the Seatbelt socket path instead of a malformed fixture identity.
	var helperPID int
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(helperPIDFile)
		if readErr != nil {
			return false
		}
		helperPID, readErr = strconv.Atoi(strings.TrimSpace(string(raw)))
		return readErr == nil && helperPID > 1
	}, 5*time.Second, 10*time.Millisecond,
		"darwin smoke helper did not publish its PID")
	row, err := db.LoadSession(darwinSmokeSessionID)
	require.NoError(t, err)
	row.PID = helperPID
	row.UpdatedAt = time.Now()
	require.NoError(t, db.SaveSession(row))
	_, err = gateWriter.Write([]byte{1})
	require.NoError(t, err)
	require.NoError(t, gateWriter.Close())

	err = cmd.Wait()
	waited = true
	if ctx.Err() != nil {
		t.Fatal("darwin tclaude-layer smoke timed out")
	}
	require.NoErrorf(t, err, "darwin tclaude-layer smoke output: %s", output.String())
}

func TestTclaudeLayerDarwinSmokeHelper(t *testing.T) {
	if os.Getenv(darwinSmokeHelperEnv) != "1" {
		t.Skip("darwin host-smoke helper subprocess")
	}
	allowed := os.Getenv(darwinSmokeAllowedEnv)
	outside := os.Getenv(darwinSmokeOutsideEnv)
	readonly := os.Getenv(darwinSmokeReadonlyEnv)
	hidden := os.Getenv(darwinSmokeHiddenEnv)
	aliasFile := os.Getenv(darwinSmokeAliasFileEnv)
	protectedFile := os.Getenv(darwinSmokeProtectedFileEnv)
	policySocket := os.Getenv(darwinSmokePolicySocketEnv)
	tmuxSocket := os.Getenv(darwinSmokeTmuxSocketEnv)
	tclaudeBinary := os.Getenv(darwinSmokeTclaudeBinaryEnv)
	restrictBaseline := os.Getenv(darwinSmokeRestrictBaselineEnv) == "1"
	exerciseBroker := os.Getenv(darwinSmokeExerciseBrokerEnv) == "1"
	networkIsolated := os.Getenv(darwinSmokeNetworkIsolatedEnv) == "1"
	hostListener := os.Getenv(darwinSmokeHostListenerEnv)
	expectedAgentID := os.Getenv(darwinSmokeExpectedAgentIDEnv)
	runtimeTempDir := os.Getenv(darwinSmokeRuntimeTempDirEnv)
	inheritedFD := os.Getenv(darwinSmokeInheritedFDEnv)
	helperPIDFile := os.Getenv(darwinSmokeHelperPIDFileEnv)
	helperGateFD := os.Getenv(darwinSmokeHelperGateFDEnv)

	require.NoError(t, os.WriteFile(
		helperPIDFile,
		[]byte(strconv.Itoa(os.Getpid())),
		0o600,
	))
	gateFD, err := strconv.Atoi(helperGateFD)
	require.NoError(t, err)
	gate := os.NewFile(uintptr(gateFD), "darwin-smoke-helper-gate")
	require.NotNil(t, gate)
	_, err = io.ReadFull(gate, make([]byte, 1))
	require.NoError(t, err)
	require.NoError(t, gate.Close())

	require.NoError(t, os.WriteFile(filepath.Join(allowed, "written"), []byte("ok"), 0o600),
		"launch-contract write root must survive an ordinary ancestor hide")
	require.Equal(t, "readonly", mustReadDarwinSmokeFile(t, filepath.Join(readonly, "host-file")))
	require.Equal(t, "alias-ok", mustReadDarwinSmokeFile(t, aliasFile),
		"the symlink spelling and resolved target must both remain usable")

	assertSeatbeltEPERM(t, os.WriteFile(filepath.Join(readonly, "blocked"), []byte("no"), 0o600),
		"RO region write")
	_, err = os.ReadFile(filepath.Join(hidden, "private"))
	assertSeatbeltEPERM(t, err, "hidden region read")
	assertSeatbeltEPERM(t, os.WriteFile(filepath.Join(hidden, "phantom"), []byte("no"), 0o600),
		"hidden region phantom write")
	_, err = os.ReadFile(filepath.Join(outside, "host-file"))
	assertSeatbeltEPERM(t, err, "ordinary ancestor hide read")
	assertSeatbeltEPERM(t, os.WriteFile(filepath.Join(outside, "phantom"), []byte("no"), 0o600),
		"ordinary ancestor hide write")
	_, err = os.ReadFile(protectedFile)
	assertSeatbeltEPERM(t, err, "protected state read")
	assertSeatbeltEPERM(t,
		os.WriteFile(filepath.Join(filepath.Dir(protectedFile), "phantom"), []byte("no"), 0o600),
		"protected state phantom write",
	)
	if conn, dialErr := net.DialTimeout("unix", policySocket, 250*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		t.Fatal("ordinary policy-hide socket remained connectable")
	} else {
		assertSeatbeltEPERM(t, dialErr, "ordinary policy-hide Unix connect")
	}
	if conn, dialErr := net.DialTimeout("unix", tmuxSocket, 250*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		t.Fatal("host tmux socket remained reachable despite class-4 deny")
	}

	require.NotEmpty(t, hostListener)
	hostConn, hostDialErr := net.DialTimeout("tcp4", hostListener, 500*time.Millisecond)
	if networkIsolated {
		if hostDialErr == nil {
			_ = hostConn.Close()
			t.Fatal("darwin isolated posture reached a host-loopback listener")
		}
		assertSeatbeltEPERM(t, hostDialErr, "host-loopback TCP connect")

		publicConn, publicDialErr := net.DialTimeout("tcp4", "1.1.1.1:53", 500*time.Millisecond)
		if publicDialErr == nil {
			_ = publicConn.Close()
			t.Fatal("darwin isolated posture reached a public DNS endpoint")
		}
		assertSeatbeltEPERM(t, publicDialErr, "public-DNS TCP connect")

		localListener, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		if listenErr == nil {
			_ = localListener.Close()
			t.Fatal("darwin isolated posture created a host-loopback listener")
		}
		assertSeatbeltEPERM(t, listenErr, "host-loopback TCP bind")
	} else {
		require.NoError(t, hostDialErr, "host-open posture must retain host-loopback connectivity")
		require.NoError(t, hostConn.Close())
	}

	devNull, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if restrictBaseline {
		assertSeatbeltEPERM(t, err, "profile RO /dev must override the baseline /dev/null carveout")
	} else {
		require.NoError(t, err, "baseline /dev/null write carveout")
		_, err = devNull.Write([]byte("probe"))
		require.NoError(t, err, "baseline /dev/null write")
		require.NoError(t, devNull.Close())
	}

	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if restrictBaseline {
		assertSeatbeltEPERM(t, err, "profile RO /dev must override the baseline tty/pty carveout")
	} else {
		require.NoError(t, err, "baseline tty/pty writable-open carveout")
		require.NoError(t, pty.Close())
	}

	inheritedPath := filepath.Join("/dev/fd", inheritedFD)
	inheritedFile, err := os.OpenFile(inheritedPath, os.O_WRONLY, 0)
	if restrictBaseline {
		assertSeatbeltEPERM(t, err, "profile RO /dev must override the baseline /dev/fd carveout")
	} else {
		require.NoError(t, err, "baseline /dev/fd writable-open carveout")
		_, err = inheritedFile.Write([]byte("probe"))
		require.NoError(t, err, "baseline /dev/fd write")
		require.NoError(t, inheritedFile.Close())
	}

	runtimeProbe := filepath.Join(runtimeTempDir, "tclaude-seatbelt-runtime-probe")
	_ = os.Remove(runtimeProbe)
	err = os.WriteFile(runtimeProbe, []byte("probe"), 0o600)
	if restrictBaseline {
		assertSeatbeltEPERM(t, err, "profile RO TMPDIR must override the baseline runtime carveout")
	} else {
		require.NoError(t, err, "baseline TMPDIR write carveout")
		require.NoError(t, os.Remove(runtimeProbe))
	}

	if !exerciseBroker {
		return
	}
	require.True(t, agentipc.SocketReachable(agentipc.ClientSocketPath()),
		"the agentd socket reopened beneath an ancestor hide must remain connectable")
	whoami, err := exec.Command(tclaudeBinary, "agent", "whoami").CombinedOutput()
	require.NoErrorf(t, err, "authenticated tclaude agent whoami through Seatbelt: %s", whoami)
	require.NotEmpty(t, expectedAgentID)
	whoamiID := strings.SplitN(strings.TrimSpace(string(whoami)), "\t", 2)[0]
	assert.Equal(t, expectedAgentID, whoamiID,
		"agentd must return the true stable identity, never a fallback identity")

	hookPayload := `{"session_id":"` + darwinSmokeConvID +
		`","cwd":"` + allowed +
		`","hook_event_name":"UserPromptSubmit","prompt":"seatbelt broker smoke"}`
	hook := exec.Command(tclaudeBinary, "session", "hook-callback")
	hook.Stdin = strings.NewReader(hookPayload)
	hookOutput, err := hook.CombinedOutput()
	require.NoErrorf(t, err, "brokered hook callback through Seatbelt: %s", hookOutput)

	statusModel := "Opus 5"
	if networkIsolated {
		statusModel += " isolated"
	}
	statusPayload := `{"session_id":"` + darwinSmokeConvID +
		`","model":{"id":"claude-opus-5","display_name":"` + statusModel + `"},` +
		`"workspace":{"current_dir":"` + allowed + `"},` +
		`"context_window":{"used_percentage":42,"context_window_size":200000},` +
		`"cost":{"total_cost_usd":1.25},"effort":{"level":"high"}}`
	status := exec.Command(tclaudeBinary, "status-bar")
	status.Stdin = strings.NewReader(statusPayload)
	statusOutput, err := status.CombinedOutput()
	require.NoErrorf(t, err, "brokered status line through Seatbelt: %s", statusOutput)
}

func assertSeatbeltEPERM(t *testing.T, err error, operation string) {
	t.Helper()
	require.Error(t, err, "%s unexpectedly succeeded", operation)
	assert.True(t, errors.Is(err, syscall.EPERM),
		"%s must fail with Seatbelt EPERM, got %v", operation, err)
}

func mustReadDarwinSmokeFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func startDarwinSeatbeltSmokeAgentd(t *testing.T, tclaudeBinary, socket string) func() {
	t.Helper()
	db.ResetForTest()
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, tclaudeBinary, "agentd", "serve",
		"--socket", socket, "--no-tray", "--no-print-human-token")
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	deadline := time.Now().Add(15 * time.Second)
	for !agentipc.SocketReachable(socket) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("smoke agentd exited during startup: %s", output.String())
		}
		if time.Now().After(deadline) {
			cancel()
			_ = cmd.Wait()
			t.Fatalf("smoke agentd did not become reachable: %s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		cancel()
		_ = cmd.Wait()
		db.ResetForTest()
	}
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func darwinSmokeIdentityEquivalentSpelling(t *testing.T, canonical, candidate string) string {
	t.Helper()
	canonicalInfo, err := os.Lstat(canonical)
	require.NoError(t, err)
	candidateInfo, err := os.Lstat(candidate)
	if err == nil && os.SameFile(canonicalInfo, candidateInfo) {
		t.Logf("exercising identity-equivalent policy spelling %q for %q", candidate, canonical)
		return candidate
	}
	t.Logf("volume keeps %q distinct or unavailable; exercising canonical spelling %q", candidate, canonical)
	return canonical
}

func copyDarwinSmokeBinary(t *testing.T, source, destination string) {
	t.Helper()
	src, err := os.Open(source)
	require.NoError(t, err)
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, err = io.Copy(dst, src)
	require.NoError(t, err)
	require.NoError(t, dst.Close())
}
