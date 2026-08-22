//go:build linux

package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
	"golang.org/x/sys/unix"
)

const stableSocketReconnectHelperEnv = "TCLAUDE_STABLE_SOCKET_RECONNECT_HELPER"

func TestConstructedRootStableSocketReconnectsAfterReplacement(t *testing.T) {
	if os.Getenv(stableSocketReconnectHelperEnv) == "1" {
		for attempt := 1; attempt <= 2; attempt++ {
			conn, err := net.DialTimeout("unix", agentipc.ClientSocketPath(), time.Second)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			fmt.Printf("connected-%d\n", attempt)
			if attempt == 1 {
				_, err = bufio.NewReader(os.Stdin).ReadString('\n')
				require.NoError(t, err)
			}
		}
		return
	}

	bwrap, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootConstructed)
	if err != nil {
		t.Skipf("bubblewrap constructed roots unavailable: %v", err)
	}
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	canonical := agentipc.CanonicalSocketPath()
	stable := agentipc.SandboxSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(stable), 0o700))
	canonicalListener, err := net.Listen("unix", canonical)
	require.NoError(t, err)
	t.Cleanup(func() { _ = canonicalListener.Close() })
	stableListener, err := net.Listen("unix", stable)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stableListener.Close() })

	workspace := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	helper := filepath.Join(workspace, "socket-reconnect-helper")
	copyTestBinary(t, os.Args[0], helper)
	args, err := bwrapArgsWithDaemonFinal([]string{workspace}, sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkHostOpen,
		RootPosture:    sandboxpolicy.RootConstructed,
		Entries: []sandboxpolicy.MountEntry{{
			Path: workspace, Mode: sandboxpolicy.MountRW,
		}},
	}, nil, nil, nil, sandboxpolicy.AgentdSocketFloor(), "", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrap,
		append(args, "--", helper,
			"-test.run=^TestConstructedRootStableSocketReconnectsAfterReplacement$")...)
	cmd.Env = append(os.Environ(),
		stableSocketReconnectHelperEnv+"=1",
		agentipc.SocketEnv+"="+stable,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	require.NoErrorf(t, err, "first sandbox connection: %s", stderr.String())
	assert.Equal(t, "connected-1\n", line)

	require.NoError(t, stableListener.Close())
	stableListener, err = net.Listen("unix", stable)
	require.NoError(t, err)
	_, err = fmt.Fprintln(stdin, "reconnect")
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	line, err = reader.ReadString('\n')
	require.NoErrorf(t, err, "replacement sandbox connection: %s", stderr.String())
	assert.Equal(t, "connected-2\n", line)
	require.NoErrorf(t, cmd.Wait(), "socket replacement helper: %s", stderr.String())
}

const (
	relayFakeBwrapEnv    = "TCLAUDE_RELAY_FAKE_BWRAP"
	relayFakeChildEnv    = "TCLAUDE_RELAY_FAKE_CHILD"
	relayFakeReporterEnv = "TCLAUDE_RELAY_FAKE_REPORTER"
	relayFakeReadyEnv    = "TCLAUDE_RELAY_FAKE_READY"
	relayFakeResizedEnv  = "TCLAUDE_RELAY_FAKE_RESIZED"
	relayFakeTestBinEnv  = "TCLAUDE_RELAY_FAKE_TEST_BINARY"
)

// A Codex-only host may never have created Claude's per-process session-state
// directory. The outer layer still hides that cross-harness protected root,
// and bubblewrap requires the target to exist before its read-only root is
// established. Preparing a Codex launch must therefore create the mountpoint
// without adding it to Codex's writable launch contract.
func TestPrepareTclaudeLayerHarnessStateCreatesCrossHarnessProtectedMountpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.CodexName,
		Cwd:         cwd,
	})
	require.NoError(t, err)
	claudeSessions := filepath.Join(home, ".claude", "sessions")
	assert.NoDirExists(t, claudeSessions)

	require.NoError(t, PrepareTclaudeLayerHarnessState(spec))
	assert.DirExists(t, claudeSessions)
	assert.NotContains(t, spec.Contract.WriteDirs, claudeSessions,
		"the host mountpoint must remain outside Codex's writable launch contract")
}

func TestResolveTclaudeLayerRefusesMissingBwrapAndRecordsOffVerdict(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	lookPathBwrap = func(string) (string, error) {
		return "", errors.New("executable file not found")
	}

	_, verdict, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorContains(t, err, "requires bubblewrap (`bwrap`) on PATH")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, "tclaude-layer unavailable", verdict.Source)

	row := toRow(&SessionState{
		ID:              "refused",
		OSSandboxState:  verdict.State,
		OSSandboxSource: verdict.Source,
	})
	assert.Equal(t, "off", row.OSSandboxState)
	assert.Equal(t, "tclaude-layer unavailable", row.OSSandboxSource)
}

func TestStackedRelayBindsRehashedOpenEngineDescriptor(t *testing.T) {
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		codexBindingTestExecutable(t),
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	args, files, err := prepareStackedRelayBinding(stackedRelayBindingOptions{
		ManifestPath:   proof.ManifestPath,
		ManifestSHA256: proof.ManifestSHA256,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, file := range files {
			_ = file.Close()
		}
	})
	require.NotEmpty(t, files)
	seals, err := unix.FcntlInt(files[0].Fd(), unix.F_GET_SEALS, 0)
	require.NoError(t, err)
	assert.Equal(t,
		unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE,
		seals,
	)
	joined := strings.Join(args, " ")
	assert.Contains(t, joined,
		"--tmpfs "+stackedBoundCodexRuntimeRoot)
	assert.Contains(t, joined,
		"--perms 0500 --file 4 "+proof.Executable.Path)
	assert.Contains(t, joined,
		filepath.Join(stackedBoundCodexRuntimeRoot, "codex-resources", "bwrap"))
	assert.Contains(t, joined,
		"--remount-ro "+stackedBoundCodexRuntimeRoot)
	assert.NotContains(t, joined,
		"--ro-bind-data 4 "+proof.Executable.Path)
	assert.NotContains(t, joined, "/proc/self/fd/")
}

func TestStackedRelayRefusesChangedManifestAuthority(t *testing.T) {
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		codexBindingTestExecutable(t),
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	manifest := readStackedBindingManifest(t, proof.ManifestPath)
	manifest.Engine.SHA256 = strings.Repeat("0", 64)
	writeStackedBindingManifest(t, proof.ManifestPath, manifest)

	_, files, err := prepareStackedRelayBinding(stackedRelayBindingOptions{
		ManifestPath:   proof.ManifestPath,
		ManifestSHA256: proof.ManifestSHA256,
	})
	for _, file := range files {
		_ = file.Close()
	}
	require.ErrorContains(t, err, "manifest changed after capability probe")
}

func TestStackedRelayOmitsConsumedProbeHelperFromFinalClaudeLaunch(t *testing.T) {
	managed := t.TempDir()
	restore := harness.SetClaudeManagedSettingsRootForTest(managed)
	t.Cleanup(restore)
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.DefaultName),
		harness.NestedSandboxExecutable{
			Path:    os.Args[0],
			Version: "test",
		},
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	require.NoError(t, proof.completeProbe())
	manifest := readStackedBindingManifest(t, proof.ManifestPath)
	require.Nil(t, manifest.ProbeHelper)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.Equal(t, proof.ManifestSHA256, stackedBindingDigest(data))

	args, files, err := prepareStackedRelayBinding(stackedRelayBindingOptions{
		ManifestPath:   proof.ManifestPath,
		ManifestSHA256: proof.ManifestSHA256,
	})
	require.NoError(t, err)
	for _, file := range files {
		_ = file.Close()
	}
	joined := strings.Join(args, " ")
	assert.NotContains(t, joined, probehelper.BoundPath)
	assert.Contains(t, joined, "--perms 0500 --file 4 "+proof.Executable.Path)
}

func TestStackedRelayRefusesReadinessInsideConsumedStageRoot(t *testing.T) {
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		codexBindingTestExecutable(t),
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	_, files, err := prepareStackedRelayBinding(stackedRelayBindingOptions{
		ManifestPath:   proof.ManifestPath,
		ManifestSHA256: proof.ManifestSHA256,
		Consume:        true,
		ReadyPath:      filepath.Join(proof.stageRoot, "ready"),
	})
	for _, file := range files {
		_ = file.Close()
	}
	require.ErrorContains(t, err, "readiness path must be outside the staging root")
}

func TestWriteStackedBindingReadyRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "ready")
	require.NoError(t, os.Symlink(target, link))
	require.Error(t, writeStackedBindingReady(link))
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStackedRelayCreatesFreshClaudePolicyRootWithoutHostDirectory(t *testing.T) {
	managed := t.TempDir()
	restore := harness.SetClaudeManagedSettingsRootForTest(managed)
	t.Cleanup(restore)
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.DefaultName),
		harness.NestedSandboxExecutable{
			Path:    os.Args[0],
			Version: "test",
		},
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	args, files, err := prepareStackedRelayBinding(stackedRelayBindingOptions{
		ManifestPath:   proof.ManifestPath,
		ManifestSHA256: proof.ManifestSHA256,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, file := range files {
			_ = file.Close()
		}
	})
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "--tmpfs /etc")
	assert.Contains(t, joined, "--dir /etc/claude-code")
	assert.Contains(t, joined, "--remount-ro /etc")
	assert.NotContains(t, joined, "--tmpfs /etc/claude-code")
	assert.Contains(t, joined,
		"--tmpfs "+stackedBoundExecutableRoot)
	assert.Contains(t, joined,
		"--perms 0500 --file 4 "+proof.Executable.Path)
	assert.Contains(t, joined,
		"--perms 0500 --file 5 "+probehelper.BoundPath)
	assert.Contains(t, joined,
		"--remount-ro "+stackedBoundExecutableRoot)
	assert.NotContains(t, joined, "/proc/self/fd/")
}

func TestResolveTclaudeLayerRefusesUnavailableUserNamespace(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error {
		return errors.New("operation not permitted")
	}

	_, verdict, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorContains(t, err, "unprivileged user namespaces may be unavailable")
	assert.Equal(t, "off", verdict.State)
}

func TestResolveTclaudeLayerRefusesUnavailableIsolatedNamespaces(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	var probed []sandboxpolicy.NetworkPosture
	probeBwrap = func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture) error {
		probed = append(probed, posture)
		if posture == sandboxpolicy.NetworkIsolatedWithAgentd {
			return errors.New("operation not permitted")
		}
		return nil
	}

	_, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.NoError(t, err)
	_, verdict, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed)
	require.ErrorContains(t, err, "mount, network, PID, and IPC namespaces")
	require.ErrorContains(t, err, "read-only remount support")
	// The cgroup namespace is probed and launched with --unshare-cgroup-try, so
	// no host is refused over it and the refusal must not claim otherwise.
	require.NotContains(t, err.Error(), "cgroup")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.NetworkIsolatedWithAgentd,
	}, probed)
}

func TestResolveTclaudeLayerNamesFilteredNamespaceRequirement(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error {
		return errors.New("operation not permitted")
	}

	_, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	require.ErrorContains(t, err, "required by filtered network")
	require.NotContains(t, err.Error(), "required by isolated-with-agentd")
}

func TestResolveTclaudeLayerRefusesUnavailablePidfdRelay(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	oldPidfdProbe := probeTclaudeLayerPidfd
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
		probeTclaudeLayerPidfd = oldPidfdProbe
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error { return nil }
	probeTclaudeLayerPidfd = func() error { return syscall.ENOSYS }

	_, verdict, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorContains(t, err, "requires Linux pidfd support")
	assert.Equal(t, "off", verdict.State)
}

func TestResolveTclaudeLayerServerDoesNotRequirePidfdRelay(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	oldPidfdProbe := probeTclaudeLayerPidfd
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
		probeTclaudeLayerPidfd = oldPidfdProbe
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error { return nil }
	probeTclaudeLayerPidfd = func() error { return syscall.ENOSYS }

	binary, verdict, err := ResolveTclaudeLayerServer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/bwrap", binary)
	assert.Equal(t, "on", verdict.State)
}

func TestTclaudeLayerCommandKeepsNewSessionBehindWinchRelay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	got, err := tclaudeLayerCommand(
		"/usr/bin/bwrap", nil, nil, nil, nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkHostOpen},
		"exec agent --flag",
	)
	require.NoError(t, err)
	relay := strings.Index(got, "session "+tclaudeLayerWinchRelayCommand)
	bwrap := strings.Index(got, "/usr/bin/bwrap")
	require.NotEqual(t, -1, relay)
	require.NotEqual(t, -1, bwrap)
	assert.Less(t, relay, bwrap)
	assert.Contains(t, got, "--new-session",
		"the resize fix must preserve bubblewrap's input-injection defense")
}

func TestTclaudeLayerServerCommandOmitsTerminalRelay(t *testing.T) {
	got, err := tclaudeLayerServerCommand(
		"/usr/bin/bwrap", nil, nil, nil, nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkHostOpen},
		"exec opencode serve",
	)
	require.NoError(t, err)
	assert.NotContains(t, got, tclaudeLayerWinchRelayCommand)
	assert.Contains(t, got, "/usr/bin/bwrap")
	assert.Contains(t, got, "--new-session")
	assert.Contains(t, got, "exec opencode serve")
}

func TestTclaudeLayerWinchRelaySignalsDescendantGroupAndPreservesExit(t *testing.T) {
	if os.Getenv(relayFakeBwrapEnv) == "1" {
		runRelayFakeBwrap(t)
		return
	}
	if os.Getenv(relayFakeChildEnv) == "1" {
		runRelayFakeChild(t)
		return
	}
	if os.Getenv(relayFakeReporterEnv) == "1" {
		runRelayFakeReporter(t)
		return
	}

	root := t.TempDir()
	fakeBwrap := root + "/fake-bwrap"
	require.NoError(t, os.WriteFile(fakeBwrap, []byte(
		"#!/bin/sh\nexec \"$"+relayFakeTestBinEnv+"\" -test.run=^TestTclaudeLayerWinchRelaySignalsDescendantGroupAndPreservesExit$\n",
	), 0o700))
	ready := root + "/ready"
	resized := root + "/resized"
	t.Setenv(relayFakeBwrapEnv, "1")
	t.Setenv(relayFakeTestBinEnv, os.Args[0])
	t.Setenv(relayFakeReadyEnv, ready)
	t.Setenv(relayFakeResizedEnv, resized)

	winch := make(chan os.Signal, 1)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := runTclaudeLayerWinchRelay(
			[]string{fakeBwrap},
			winch,
			stackedRelayBindingOptions{},
		)
		done <- result{code: code, err: err}
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)
	winch <- syscall.SIGWINCH
	require.Eventually(t, func() bool {
		_, err := os.Stat(resized)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond,
		"SIGWINCH did not reach the fake bwrap child's descendant process")

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, 37, got.code, "relay must preserve the sandbox exit status")
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not return after its child exited")
	}
}

func runRelayFakeBwrap(t *testing.T) {
	t.Helper()
	_, err := unix.Setsid()
	require.NoError(t, err)
	status := os.NewFile(3, "bwrap-status")
	require.NotNil(t, status)

	cmd := exec.Command(os.Args[0], "-test.run=^TestTclaudeLayerWinchRelaySignalsDescendantGroupAndPreservesExit$")
	cmd.Env = append(os.Environ(), relayFakeBwrapEnv+"=0", relayFakeChildEnv+"=1")
	cmd.ExtraFiles = []*os.File{status}
	require.NoError(t, cmd.Run())
	os.Exit(37)
}

func runRelayFakeChild(t *testing.T) {
	t.Helper()
	status := os.NewFile(3, "bwrap-status")
	require.NotNil(t, status)
	_, err := fmt.Fprintf(status, "{\"child-pid\":%d}\n", os.Getpid())
	require.NoError(t, err)
	require.NoError(t, status.Close())

	// Bubblewrap emits child-pid before --new-session finishes the child's
	// process-group transition. Reproduce that ordering so a relay that pins
	// the pre-transition pgid cannot satisfy this regression.
	_, err = unix.Setsid()
	require.NoError(t, err)

	cmd := exec.Command(os.Args[0], "-test.run=^TestTclaudeLayerWinchRelaySignalsDescendantGroupAndPreservesExit$")
	cmd.Env = append(os.Environ(), relayFakeChildEnv+"=0", relayFakeReporterEnv+"=1")
	require.NoError(t, cmd.Run())
	os.Exit(0)
}

func runRelayFakeReporter(t *testing.T) {
	t.Helper()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	require.NoError(t, os.WriteFile(os.Getenv(relayFakeReadyEnv), []byte("ready"), 0o600))
	select {
	case <-winch:
	case <-time.After(3 * time.Second):
		t.Fatal("fake descendant did not receive SIGWINCH")
	}
	require.NoError(t, os.WriteFile(os.Getenv(relayFakeResizedEnv), []byte("resized"), 0o600))
}

// The probe backs the dashboard's polled capability disclosure as well as the
// launch refusal, so a namespace the launch takes but the probe does not would
// surface as a green capability beside a failing launch — and one the probe
// takes but the launch does not would refuse hosts for nothing. Both renderers
// therefore read the ambient namespaces from tclaudeLayerAmbientNamespaceArgs,
// and this pins that they both actually carry everything it returns. Driving
// the assertion off the helper rather than a literal list is deliberate: a
// third flag added there is covered without touching this test.
func TestAmbientNamespacesAppearInBothTheLaunchAndTheProbe(t *testing.T) {
	ambient := tclaudeLayerAmbientNamespaceArgs()
	require.NotEmpty(t, ambient)

	// A constructed root refuses to render without the agentd socket it binds
	// back, so stand one up rather than letting the case depend on whether a
	// daemon happens to be running on the machine under test.
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	for _, floorSocket := range sandboxpolicy.AgentdSocketFloor() {
		require.NoError(t, os.MkdirAll(filepath.Dir(floorSocket), 0o700))
		listener, listenErr := net.Listen("unix", floorSocket)
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = listener.Close() })
	}

	for _, tc := range []struct {
		name    string
		posture sandboxpolicy.NetworkPosture
		root    sandboxpolicy.RootPosture
	}{
		{"host-open-inherited-root", sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited},
		{"host-open-constructed-root", sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootConstructed},
		{"isolated-with-agentd", sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.RootConstructed},
		{"filtered", sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe, err := tclaudeLayerProbeArgs(tc.posture, tc.root)
			require.NoError(t, err)
			for _, flag := range ambient {
				assert.Contains(t, probe, flag, "the probe must build what the launch will")
			}

			if tc.posture == sandboxpolicy.NetworkFiltered {
				// The filtered launch needs a compiled gateway policy that this
				// table cannot cheaply synthesize. It is not a coverage hole:
				// the ambient args are appended in the shared prologue, before
				// the posture switch, so the three plans below exercise the
				// same line. Its probe is covered above like the others.
				return
			}
			launch, err := bwrapArgs(nil, sandboxpolicy.MountPlan{
				NetworkPosture: tc.posture,
				RootPosture:    tc.root,
			})
			require.NoError(t, err)
			for _, flag := range ambient {
				assert.Contains(t, launch, flag, "the launch must take what the probe proved")
			}
		})
	}
}

func TestTclaudeLayerProbeExercisesReadOnlyRemountSemantics(t *testing.T) {
	for _, posture := range []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.NetworkIsolatedWithAgentd,
	} {
		t.Run(posture.String(), func(t *testing.T) {
			args, err := tclaudeLayerProbeArgs(posture,
				sandboxpolicy.RootPostureFor(posture, sandboxpolicy.AccessModeUnset))
			require.NoError(t, err)

			tmpfs := indexOfBwrapTriplet(args, "--tmpfs", "/tmp")
			childBind := indexOfBwrapTriplet(args, "--ro-bind", "/dev/null")
			remount := indexOfBwrapTriplet(args, "--remount-ro", "/tmp")
			require.NotEqual(t, -1, tmpfs)
			require.NotEqual(t, -1, childBind)
			require.NotEqual(t, -1, remount)
			assert.Less(t, tmpfs, childBind)
			assert.Less(t, childBind, remount)
			assert.Contains(t, args[len(args)-1], "! touch /tmp/.tclaude-remount-write")
		})
	}
}
