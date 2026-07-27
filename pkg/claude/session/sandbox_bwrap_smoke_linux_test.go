//go:build linux

package session

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"golang.org/x/sys/unix"
)

const (
	tclaudeLayerSmokeHelperEnv = "TCLAUDE_SANDBOX_V2_SMOKE_HELPER"
	smokeAllowedEnv            = "TCLAUDE_SANDBOX_V2_ALLOWED"
	smokeOutsideEnv            = "TCLAUDE_SANDBOX_V2_OUTSIDE"
	smokeAliasFileEnv          = "TCLAUDE_SANDBOX_V2_ALIAS_FILE"
	smokeProtectedFileEnv      = "TCLAUDE_SANDBOX_V2_PROTECTED_FILE"
	smokeProtectedReadableEnv  = "TCLAUDE_SANDBOX_V2_PROTECTED_READABLE"
	smokeTmuxSocketEnv         = "TCLAUDE_SANDBOX_V2_TMUX_SOCKET"
	smokeRuntimeSocketEnv      = "TCLAUDE_SANDBOX_V2_RUNTIME_SOCKET"
	smokeHostPIDEnv            = "TCLAUDE_SANDBOX_V2_HOST_PID"
	smokeLoopbackAddrEnv       = "TCLAUDE_SANDBOX_V2_LOOPBACK_ADDR"
	smokeTclaudeBinaryEnv      = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	smokeResizeReporterEnv     = "TCLAUDE_SANDBOX_V2_RESIZE_REPORTER"
	smokePrivateOwnFileEnv     = "TCLAUDE_SANDBOX_V2_PRIVATE_OWN_FILE"
	smokePrivateSiblingDirEnv  = "TCLAUDE_SANDBOX_V2_PRIVATE_SIBLING_DIR"
)

const smokeConvID = "75000000-0000-4000-8000-000000000750"

func TestTclaudeLayerHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_V2_SMOKE") != "1" {
		t.Skip("set TCLAUDE_SANDBOX_V2_SMOKE=1 on an unsandboxed Linux host with bubblewrap")
	}
	binary, _, err := ResolveTclaudeLayer(sandboxpolicy.NetworkIsolatedWithAgentd)
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(home, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-sandbox-v2-smoke-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	realTools := filepath.Join(root, "real-tools")
	aliasTools := filepath.Join(root, "alias-tools")
	smokeHome := filepath.Join(root, "home")
	protectedDir := filepath.Join(smokeHome, ".tclaude", "data")
	protectedFile := filepath.Join(protectedDir, "private")
	tmuxBase := filepath.Join(root, "tmux-base")
	// Production attachment roots live beneath the class-3 daemon-data hide.
	// The smoke therefore proves the daemon-only child carveout works through
	// that protected ancestor, not merely under an otherwise visible parent.
	privateParent := filepath.Join(protectedDir, "spawn-attachments")
	privateOwn := filepath.Join(privateParent, "own-session")
	privateSibling := filepath.Join(privateParent, "sibling-session")
	for _, dir := range []string{
		allowed, outside, realTools, filepath.Join(smokeHome, ".tclaude", "api"), protectedDir, tmuxBase,
		filepath.Join(smokeHome, ".claude", "sessions"), privateOwn, privateSibling,
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	privateOwnFile := filepath.Join(privateOwn, "pasted-image.png")
	require.NoError(t, os.WriteFile(privateOwnFile, []byte("own-session-image"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(privateSibling, "sibling-secret.png"),
		[]byte("sibling-session-image"),
		0o600,
	))
	t.Setenv("HOME", smokeHome)
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	tclaudeBinary := strings.TrimSpace(os.Getenv(smokeTclaudeBinaryEnv))
	require.NotEmpty(t, tclaudeBinary,
		"set TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY to the current built tclaude binary")
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	agentSocket := filepath.Join(smokeHome, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startTclaudeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)
	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    "sandbox-v2-smoke",
		PID:                   os.Getpid(),
		ConvID:                smokeConvID,
		Harness:               harness.DefaultName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		Cwd:                   allowed,
		Status:                StatusWorking,
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmuxSocketDir, 0o700))
	tmuxSocket := filepath.Join(tmuxSocketDir, "tclaude")
	tmuxListener, err := net.Listen("unix", tmuxSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tmuxListener.Close() })
	runtimeSocket := filepath.Join(root, "runtime", "ambient.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(runtimeSocket), 0o700))
	runtimeListener, err := net.Listen("unix", runtimeSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtimeListener.Close() })
	hostLoopback, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = hostLoopback.Close() })
	require.NoError(t, os.WriteFile(protectedFile, []byte("must-stay-hidden"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(realTools, "probe"), []byte("alias-ok"), 0o600))
	require.NoError(t, os.Symlink(realTools, aliasTools))
	// `go test` normally places its executable under /tmp, which this layer
	// intentionally replaces with a fresh tmpfs. Copy the helper alongside the
	// fixture so the sandbox can execute it through the read-only base root.
	// Name the helper `claude`: the real agentd identity resolver recognizes
	// that harness ancestor and resolves its PID through the sessions table.
	helperBinary := filepath.Join(allowed, "claude")
	copyTestBinary(t, os.Args[0], helperBinary)

	// Spell the profile rule through a symlink. Resolution must bind the real
	// target, while the base read-only root keeps the alias itself usable.
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Explicit: &sandboxpolicy.Profile{
			Name:          "tclaude-layer-smoke",
			NetworkAccess: sandboxpolicy.NetworkAccessNone,
			Filesystem: []sandboxpolicy.FilesystemGrant{
				// Exercise a legitimate most-specific-wins reopen beneath an
				// ordinary hide. The applier must create this child bind before
				// remounting the hidden parent read-only.
				{Path: root, Access: sandboxpolicy.AccessDeny},
				{Path: allowed, Access: sandboxpolicy.AccessWrite},
				{Path: aliasTools, Access: sandboxpolicy.AccessRead},
				{Path: filepath.Dir(tclaudeBinary), Access: sandboxpolicy.AccessRead},
				// The applier's final host-control phase must override even an
				// ordinary write grant on the tmux socket directory's parent.
				{Path: tmuxBase, Access: sandboxpolicy.AccessWrite},
			},
		},
	})
	require.NoError(t, err)
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	require.NoError(t, err)
	assert.Contains(t, plan.Entries, sandboxpolicy.MountEntry{Path: realTools, Mode: sandboxpolicy.MountRO})
	assert.NotContains(t, plan.Entries, sandboxpolicy.MountEntry{Path: aliasTools, Mode: sandboxpolicy.MountRO})
	assert.Contains(t, plan.Aliases, sandboxpolicy.MountAlias{Link: aliasTools, Target: realTools})

	phase0, err := tclaudeLayerPhase0WriteDirs(TclaudeLayerLaunchContract{
		HarnessName: harness.DefaultName,
		WriteDirs:   []string{allowed},
	}, effective)
	require.NoError(t, err)
	hostOpenPlan := plan
	hostOpenPlan.NetworkPosture = sandboxpolicy.NetworkHostOpen
	for _, tc := range []struct {
		name string
		plan sandboxpolicy.MountPlan
	}{
		{name: "host-open", plan: resizePlanClone(hostOpenPlan)},
		{name: "isolated", plan: resizePlanClone(plan)},
	} {
		t.Run("terminal-resize-"+tc.name, func(t *testing.T) {
			assertTclaudeLayerResizeRoundTrip(
				t, binary, tclaudeBinary, helperBinary, phase0, tc.plan,
			)
		})
	}
	privateWriteDir := TclaudeLayerPrivateWriteDir{Parent: privateParent, Current: privateOwn}
	runTclaudeLayerSmokeHelper(t, binary, helperBinary, phase0, plan, privateWriteDir, false, allowed, outside,
		filepath.Join(aliasTools, "probe"), protectedFile, tmuxSocket, runtimeSocket,
		strconv.Itoa(os.Getpid()), hostLoopback.Addr().String(), tclaudeBinary,
		privateOwnFile, privateSibling)

	breakGlassEffective := effective
	breakGlassEffective.BreakGlassFilesystem = []sandboxpolicy.BreakGlassGrant{{
		Path: protectedDir, Access: sandboxpolicy.AccessRead,
	}}
	breakGlassPlan, err := sandboxpolicy.RenderMountPlan(breakGlassEffective)
	require.NoError(t, err)
	runTclaudeLayerSmokeHelper(t, binary, helperBinary, phase0, breakGlassPlan, privateWriteDir, true, allowed, outside,
		filepath.Join(aliasTools, "probe"), protectedFile, tmuxSocket, runtimeSocket,
		strconv.Itoa(os.Getpid()), hostLoopback.Addr().String(), tclaudeBinary,
		privateOwnFile, privateSibling)
}

func runTclaudeLayerSmokeHelper(
	t *testing.T,
	binary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	privateWriteDir TclaudeLayerPrivateWriteDir,
	protectedReadable bool,
	allowed, outside, aliasFile, protectedFile, tmuxSocket, runtimeSocket,
	hostPID, hostLoopbackAddr, tclaudeBinary, privateOwnFile, privateSiblingDir string,
) {
	t.Helper()
	var breakGlassPaths []string
	if protectedReadable {
		breakGlassPaths = []string{filepath.Dir(protectedFile)}
	}
	args, err := bwrapArgs(phase0WriteDirs, breakGlassPaths, plan, privateWriteDir)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		append(args, "--", helperBinary, "-test.run=^TestTclaudeLayerSmokeHelper$")...)
	cmd.Env = append(os.Environ(),
		tclaudeLayerSmokeHelperEnv+"=1",
		smokeAllowedEnv+"="+allowed,
		smokeOutsideEnv+"="+outside,
		smokeAliasFileEnv+"="+aliasFile,
		smokeProtectedFileEnv+"="+protectedFile,
		smokeProtectedReadableEnv+"="+boolEnv(protectedReadable),
		smokeTmuxSocketEnv+"="+tmuxSocket,
		smokeRuntimeSocketEnv+"="+runtimeSocket,
		smokeHostPIDEnv+"="+hostPID,
		smokeLoopbackAddrEnv+"="+hostLoopbackAddr,
		smokeTclaudeBinaryEnv+"="+tclaudeBinary,
		smokePrivateOwnFileEnv+"="+privateOwnFile,
		smokePrivateSiblingDirEnv+"="+privateSiblingDir,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("tclaude-layer host smoke timed out")
	}
	require.NoErrorf(t, err, "tclaude-layer host smoke output: %s", output)
}

func resizePlanClone(plan sandboxpolicy.MountPlan) sandboxpolicy.MountPlan {
	plan.Entries = append([]sandboxpolicy.MountEntry(nil), plan.Entries...)
	plan.Aliases = append([]sandboxpolicy.MountAlias(nil), plan.Aliases...)
	return plan
}

func assertTclaudeLayerResizeRoundTrip(
	t *testing.T,
	bwrapBinary, tclaudeBinary, helperBinary string,
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
) {
	t.Helper()
	args, err := bwrapArgs(phase0WriteDirs, nil, plan)
	require.NoError(t, err)

	// Keep the reporter beneath a live sh wrapper instead of letting sh exec
	// it as its final command. This is the production topology: bubblewrap's
	// reported child is `sh -c <harness command>`, while the TUI is a
	// descendant that only a process-group signal reliably reaches.
	reporterCommand := clcommon.ShellQuoteArg(helperBinary) +
		" -test.run=^TestTclaudeLayerResizeSmokeReporter$; " +
		"tclaude_resize_status=$?; exit \"$tclaude_resize_status\""
	relayArgs := []string{"session", tclaudeLayerWinchRelayCommand, "--", bwrapBinary}
	relayArgs = append(relayArgs, args...)
	relayArgs = append(relayArgs, "--", "sh", "-c", reporterCommand)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tclaudeBinary, relayArgs...)
	cmd.Env = append(os.Environ(), smokeResizeReporterEnv+"=1")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	require.NoError(t, err)
	defer func() { _ = ptmx.Close() }()

	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			lines <- strings.TrimSpace(scanner.Text())
		}
		close(lines)
	}()
	observed := make([]string, 0, 8)
	waitLine := func(prefix string) string {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("resize reporter exited before %s; output=%q", prefix, observed)
				}
				observed = append(observed, line)
				if strings.HasPrefix(line, prefix) {
					return line
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for %s; output=%q", prefix, observed)
			}
		}
	}

	ready := waitLine("READY ")
	assert.Contains(t, ready, "rows=24 cols=80")
	assert.Contains(t, ready, "parent=sh")
	assert.Contains(t, ready, "tty_fg=ENOTTY",
		"--new-session must keep the sandbox disconnected from the host controlling tty")

	require.NoError(t, pty.Setsize(ptmx, &pty.Winsize{Rows: 37, Cols: 113}))
	resized := waitLine("RESIZED ")
	assert.Contains(t, resized, "rows=37 cols=113")

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		require.NoErrorf(t, err, "resize relay output: %q", observed)
	case <-ctx.Done():
		t.Fatalf("resize relay did not exit after reporter completed; output=%q", observed)
	}
}

func TestTclaudeLayerResizeSmokeReporter(t *testing.T) {
	if os.Getenv(smokeResizeReporterEnv) != "1" {
		t.Skip("host-smoke terminal resize reporter")
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	fd := int(os.Stdin.Fd())
	size, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	require.NoError(t, err)
	_, err = unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	require.Error(t, err, "sandbox unexpectedly reacquired a controlling tty")
	require.ErrorIs(t, err, syscall.ENOTTY)

	parentComm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(os.Getppid()), "comm"))
	require.NoError(t, err)
	fmt.Printf(
		"READY rows=%d cols=%d parent=%s tty_fg=ENOTTY pid=%d ppid=%d pgrp=%d\n",
		size.Row, size.Col, strings.TrimSpace(string(parentComm)),
		os.Getpid(), os.Getppid(), unix.Getpgrp(),
	)

	select {
	case <-winch:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapped descendant did not receive SIGWINCH")
	}
	size, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	require.NoError(t, err)
	fmt.Printf("RESIZED rows=%d cols=%d\n", size.Row, size.Col)
}

func TestTclaudeLayerSmokeHelper(t *testing.T) {
	if os.Getenv(tclaudeLayerSmokeHelperEnv) != "1" {
		t.Skip("host-smoke helper subprocess")
	}
	allowed := os.Getenv(smokeAllowedEnv)
	outside := os.Getenv(smokeOutsideEnv)
	aliasFile := os.Getenv(smokeAliasFileEnv)
	protectedFile := os.Getenv(smokeProtectedFileEnv)
	protectedReadable := os.Getenv(smokeProtectedReadableEnv) == "1"
	tmuxSocket := os.Getenv(smokeTmuxSocketEnv)
	runtimeSocket := os.Getenv(smokeRuntimeSocketEnv)
	hostPID := os.Getenv(smokeHostPIDEnv)
	hostLoopbackAddr := os.Getenv(smokeLoopbackAddrEnv)
	tclaudeBinary := os.Getenv(smokeTclaudeBinaryEnv)
	privateOwnFile := os.Getenv(smokePrivateOwnFileEnv)
	privateSiblingDir := os.Getenv(smokePrivateSiblingDirEnv)

	privateImage, err := os.ReadFile(privateOwnFile)
	require.NoError(t, err, "the current session's private attachment must be readable")
	assert.Equal(t, "own-session-image", string(privateImage))
	_, err = os.Stat(privateSiblingDir)
	require.Error(t, err, "a sibling session's private subtree must be absent")
	assert.True(t, errors.Is(err, syscall.ENOENT),
		"sibling private subtree must fail with ENOENT, got %v", err)
	require.NoError(t, os.WriteFile(filepath.Join(allowed, "written"), []byte("ok"), 0o600),
		"a writable child reopen inside an ordinary hide must stay writable")
	if err := os.WriteFile(filepath.Join(outside, "blocked"), []byte("no"), 0o600); err == nil {
		t.Fatal("write outside the allowed root unexpectedly succeeded")
	}
	deniedRootWrite := filepath.Join(filepath.Dir(outside), "blocked-at-denied-root")
	err = os.WriteFile(deniedRootWrite, []byte("must-fail"), 0o600)
	require.Error(t, err, "the topmost denied-root tmpfs must reject writes")
	assert.True(t, errors.Is(err, syscall.EROFS),
		"denied-root tmpfs write must fail with EROFS, got %v", err)
	// The parent of the smoke fixture is auto-created in the isolated
	// posture's constructed tmpfs root but is not itself an explicit mount.
	// It must be read-only rather than accepting another throwaway write.
	unboundRootWrite := filepath.Join(filepath.Dir(filepath.Dir(outside)), "tclaude-sandbox-v2-phantom")
	err = os.WriteFile(unboundRootWrite, []byte("must-fail"), 0o600)
	require.Error(t, err, "the constructed root must reject writes to unbound paths")
	assert.True(t, errors.Is(err, syscall.EROFS),
		"constructed-root write must fail with EROFS, got %v", err)
	got, err := os.ReadFile(aliasFile)
	require.NoError(t, err, "symlink alias must remain usable through the read-only base root")
	assert.Equal(t, "alias-ok", string(got))
	protected, err := os.ReadFile(protectedFile)
	if protectedReadable {
		require.NoError(t, err, "acknowledged break-glass path must reopen after the protected baseline")
		assert.Equal(t, "must-stay-hidden", string(protected))
	} else if err == nil {
		t.Fatal("protected tclaude state unexpectedly remained readable under an ordinary profile")
	}
	hiddenWrite := filepath.Join(filepath.Dir(protectedFile), "phantom")
	err = os.WriteFile(hiddenWrite, []byte("must-fail"), 0o600)
	require.Error(t, err, "a hidden path must reject writes instead of accepting phantom state")
	assert.True(t, errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EROFS),
		"hidden-path write must fail with ENOENT or EROFS, got %v", err)
	if !protectedReadable {
		err = os.MkdirAll(filepath.Dir(protectedFile), 0o700)
		require.Error(t, err, "an ancestor-denied protected path must not be creatable")
		assert.True(t, errors.Is(err, syscall.EROFS),
			"ancestor-denied protected path creation must fail with EROFS, got %v", err)
	}
	if conn, err := net.DialTimeout("unix", tmuxSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("host tmux socket remained reachable despite the final applier hide")
	}
	if conn, err := net.DialTimeout("unix", runtimeSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("ambient runtime socket remained reachable inside the constructed root")
	}
	procRootSocket := filepath.Join("/proc", hostPID, "root", strings.TrimPrefix(runtimeSocket, "/"))
	if conn, err := net.DialTimeout("unix", procRootSocket, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("ambient runtime socket remained reachable through a host process's /proc/<pid>/root")
	}
	if conn, err := net.DialTimeout("tcp4", hostLoopbackAddr, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("host-loopback TCP remained reachable across the isolated network namespace")
	}
	if conn, err := net.DialTimeout("tcp4", "1.1.1.1:53", 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("TCP egress unexpectedly succeeded inside the isolated network namespace")
	}
	loopback, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err, "loopback must be up inside the isolated network namespace")
	defer func() { _ = loopback.Close() }()
	conn, err := net.DialTimeout("tcp4", loopback.Addr().String(), 250*time.Millisecond)
	require.NoError(t, err, "namespace-local loopback round trip")
	_ = conn.Close()

	assertTclaudeLayerReapsOrphan(t)

	whoami, err := exec.Command(tclaudeBinary, "agent", "whoami").CombinedOutput()
	require.NoErrorf(t, err, "authenticated tclaude agent whoami inside namespace: %s", whoami)
	assert.True(t, strings.HasPrefix(strings.TrimSpace(string(whoami)), "agt_"),
		"agentd must resolve a stable managed identity through bwrap ancestry; got %q", whoami)
}

func assertTclaudeLayerReapsOrphan(t *testing.T) {
	t.Helper()
	output, err := exec.Command(
		"/bin/sh",
		"-c",
		"sleep 0.2 </dev/null >/dev/null 2>&1 & echo $!",
	).Output()
	require.NoError(t, err, "launch orphan-reaping probe")
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err, "parse orphan-reaping probe PID")
	procPath := filepath.Join("/proc", strconv.Itoa(pid))
	require.Eventually(t, func() bool {
		_, err := os.Stat(procPath)
		return os.IsNotExist(err)
	}, 3*time.Second, 25*time.Millisecond,
		"bubblewrap PID 1 must reap orphaned harness subprocesses")
}

func startTclaudeLayerSmokeAgentd(t *testing.T, tclaudeBinary, socket string) func() {
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

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func copyTestBinary(t *testing.T, source, destination string) {
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
