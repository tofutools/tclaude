//go:build linux

package sandboxassumptions

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	"golang.org/x/sys/unix"
)

const (
	linuxAssumptionHelperEnv = "TCLAUDE_SANDBOX_ASSUMPTION_HELPER"
)

func TestBubblewrapAssumptions(t *testing.T) {
	if os.Getenv(assumptionsGateEnv) != "1" {
		t.Skip("set TCLAUDE_SANDBOX_ASSUMPTIONS=1 on an unsandboxed Linux host with bubblewrap")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatalf("find bubblewrap: %v", err)
	}

	runLinuxAssumption(t, "NestedUserNamespaces",
		"session.resolveBwrapServerBinary, session.tclaudeLayerProbeArgs, "+
			"session.StackedSandboxAvailability, session.ProbeStackedSandbox",
		func(t *testing.T) { assumeNestedUserNamespaces(t, bwrap) })
	runLinuxAssumption(t, "NewSessionDisconnectsControllingTTY",
		"session.bwrapArgs, session.runTclaudeLayerWinchRelay, "+
			"session.signalPinnedTclaudeLayerGroup",
		func(t *testing.T) { assumeNewSessionDisconnectsTTY(t, bwrap) })
	runLinuxAssumption(t, "StatusChildBecomesSessionGroupLeader",
		"session.runTclaudeLayerWinchRelay",
		func(t *testing.T) { assumeStatusChildIdentity(t, bwrap) })
	runLinuxAssumption(t, "RemountROIsNonRecursive",
		"session.tclaudeLayerProbeArgs, session.tclaudeLayerHideRemounts.appendRemounts, "+
			"session.bwrapArgs",
		func(t *testing.T) { assumeRemountRONonRecursive(t, bwrap) })
	runLinuxAssumption(t, "PrivateParentHideAndSoleChildReopen",
		"session.bwrapArgs private-write-dir phase and contract/socket repairs",
		func(t *testing.T) { assumePrivateChildReopen(t, bwrap) })
	runLinuxAssumption(t, "SymlinkOrderingAndTargetHide",
		"session.appendTclaudeLayerAliases, session.appendTclaudeLayerAliasRepairs",
		func(t *testing.T) { assumeSymlinkOrdering(t, bwrap) })
	runLinuxAssumption(t, "DevProvidesFreshDevpts",
		"session.bwrapArgs --dev launch hygiene and the interactive harness contract",
		func(t *testing.T) { assumeDevpts(t, bwrap) })
	runLinuxAssumption(t, "NetworkAndPIDNamespaceIsolation",
		"session.bwrapArgs isolated posture and session.tclaudeLayerProbeArgs",
		func(t *testing.T) { assumeNetworkAndPIDIsolation(t, bwrap) })
	runLinuxAssumption(t, "SealedMemfdMaterializesExecutable",
		"session.prepareStackedRelayBinding exact-engine binding",
		func(t *testing.T) { assumeSealedMemfdMaterialization(t, bwrap) })
	runLinuxAssumption(t, "MissingPolicyChildUnderReadOnlyParent",
		"session.prepareStackedRelayBinding absent /etc/claude-code branch",
		func(t *testing.T) { assumeMissingPolicyChild(t, bwrap) })
}

func runLinuxAssumption(
	t *testing.T,
	name, reliance string,
	test func(*testing.T),
) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Logf("production reliance: %s", reliance)
		test(t)
	})
}

func assumeNestedUserNamespaces(t *testing.T, bwrap string) {
	t.Helper()
	inner := []string{
		bwrap,
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--", "/usr/bin/true",
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "host-open",
			args: []string{
				"--die-with-parent",
				"--ro-bind", "/", "/",
				"--dev", "/dev",
				"--proc", "/proc",
			},
		},
		{
			name: "isolated-constructed-root",
			args: constructedRootArgs(t),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string(nil), tc.args...)
			args = append(args, "--")
			args = append(args, inner...)
			runBwrap(t, bwrap, args, nil)
		})
	}
}

func constructedRootArgs(t *testing.T) []string {
	t.Helper()
	args := []string{
		"--die-with-parent",
		"--unshare-net",
		"--unshare-pid",
		"--tmpfs", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	for _, path := range []string{
		"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32", "/etc",
	} {
		info, err := os.Lstat(path)
		switch {
		case err == nil:
		case os.IsNotExist(err):
			continue
		default:
			t.Fatalf("inspect constructed-root source %s: %v", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				t.Fatalf("read constructed-root symlink %s: %v", path, readErr)
			}
			args = append(args, "--symlink", target, path)
			continue
		}
		args = append(args, "--ro-bind", path, path)
	}
	return args
}

func assumeNewSessionDisconnectsTTY(t *testing.T, bwrap string) {
	t.Helper()
	cmd := helperCommand(
		bwrap,
		[]string{
			"--die-with-parent",
			"--new-session",
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
		},
		"tty-session",
		nil,
	)
	cmd.Env = append(os.Environ(), linuxAssumptionHelperEnv+"=tty-session")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("start new-session helper under pty: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	reader := bufio.NewReader(ptmx)
	ready := readLineWithDeadline(t, reader, ptmx, "READY")
	if !strings.Contains(ready, "tty_fg=ENOTTY") {
		t.Fatalf("--new-session retained a controlling tty: %q", ready)
	}
	if !strings.Contains(ready, "rows=24 cols=80") {
		t.Fatalf("initial winsize mismatch: %q", ready)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 37, Cols: 113}); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
	// StartWithSize leaves the slave in canonical mode, so every bounded
	// handshake phase must send and consume a complete newline-delimited record.
	if _, err := ptmx.Write([]byte("probe\n")); err != nil {
		t.Fatalf("trigger helper winsize read: %v", err)
	}
	probed := readLineWithDeadline(t, reader, ptmx, "PROBED")
	if !strings.Contains(probed, "rows=37 cols=113") {
		t.Fatalf("shared pty winsize did not change: %q", probed)
	}
	if !strings.Contains(probed, "auto_winch=false") {
		t.Fatalf("detached session unexpectedly received automatic SIGWINCH: %q", probed)
	}
	if err := cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatalf("signal bwrap process: %v", err)
	}
	// Bubblewrap, not its detached child, received the signal above. The helper
	// must therefore still exit only through its explicit stdin handshake.
	if _, err := ptmx.Write([]byte("quit\n")); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	waitCommand(t, cmd, 5*time.Second)
}

func assumeStatusChildIdentity(t *testing.T, bwrap string) {
	t.Helper()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create status pipe: %v", err)
	}
	defer func() { _ = statusR.Close() }()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create helper gate: %v", err)
	}
	defer func() { _ = stdinW.Close() }()

	cmd := exec.Command(
		bwrap,
		"--json-status-fd", "3",
		"--die-with-parent",
		"--new-session",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--",
		os.Args[0],
		"-test.run=^TestBubblewrapAssumptionHelper$",
	)
	cmd.Env = append(os.Environ(), linuxAssumptionHelperEnv+"=wait-stdin")
	cmd.ExtraFiles = []*os.File{statusW}
	cmd.Stdin = stdinR
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start status-fd bwrap: %v", err)
	}
	_ = statusW.Close()
	_ = stdinR.Close()

	type childStatus struct {
		ChildPID int `json:"child-pid"`
	}
	var status childStatus
	decodeDone := make(chan error, 1)
	go func() { decodeDone <- json.NewDecoder(statusR).Decode(&status) }()
	select {
	case err := <-decodeDone:
		if err != nil {
			t.Fatalf("decode bwrap child status: %v; output=%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("timed out waiting for bwrap child status")
	}
	if status.ChildPID <= 1 {
		t.Fatalf("invalid reported child pid %d", status.ChildPID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pgid, pgidErr := syscall.Getpgid(status.ChildPID)
		if pgidErr == nil && pgid == status.ChildPID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reported child %d did not become its new-session group leader: pgid=%d err=%v",
				status.ChildPID, pgid, pgidErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	pidfd, err := unix.PidfdOpen(status.ChildPID, 0)
	if err != nil {
		t.Fatalf("pin reported bwrap child with pidfd: %v", err)
	}
	if err := unix.Close(pidfd); err != nil {
		t.Fatalf("close child pidfd: %v", err)
	}
	_ = stdinW.Close()
	waitCommand(t, cmd, 5*time.Second)
}

func assumeRemountRONonRecursive(t *testing.T, bwrap string) {
	t.Helper()
	root := canonicalTempDir(t)
	child := filepath.Join(root, "child")
	sibling := filepath.Join(root, "sibling")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatalf("create sibling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(child, "seed"), []byte("seed"), 0o600); err != nil {
		t.Fatalf("write child seed: %v", err)
	}
	env := map[string]string{
		"ASSUME_PARENT":  root,
		"ASSUME_CHILD":   child,
		"ASSUME_SIBLING": sibling,
	}
	args := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--tmpfs", root,
		"--bind", child, child,
		"--remount-ro", root,
	}
	runHelperInBwrap(t, bwrap, args, "verify-remount", env, nil)
}

func assumePrivateChildReopen(t *testing.T, bwrap string) {
	t.Helper()
	root := canonicalTempDir(t)
	current := filepath.Join(root, "current")
	sibling := filepath.Join(root, "sibling")
	for _, path := range []string{current, sibling} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(current, "own"), []byte("own"), 0o600); err != nil {
		t.Fatalf("write current fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write sibling fixture: %v", err)
	}
	env := map[string]string{
		"ASSUME_PARENT":  root,
		"ASSUME_CHILD":   current,
		"ASSUME_SIBLING": sibling,
	}
	args := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--tmpfs", root,
		"--bind", current, current,
		"--remount-ro", root,
	}
	runHelperInBwrap(t, bwrap, args, "verify-private-child", env, nil)
}

func assumeSymlinkOrdering(t *testing.T, bwrap string) {
	t.Helper()
	fixture := canonicalTempDir(t)
	target := filepath.Join(fixture, "target")
	aliasParent := filepath.Join(fixture, "aliases")
	alias := filepath.Join(aliasParent, "deep", "link")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.MkdirAll(aliasParent, 0o700); err != nil {
		t.Fatalf("create alias parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "probe"), []byte("alias-ok"), 0o600); err != nil {
		t.Fatalf("write alias fixture: %v", err)
	}
	env := map[string]string{"ASSUME_ALIAS_FILE": filepath.Join(alias, "probe")}
	base := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--tmpfs", aliasParent,
		"--symlink", target, alias,
		"--remount-ro", aliasParent,
	}
	runHelperInBwrap(t, bwrap, base, "verify-alias-readable", env, nil)

	hidden := append([]string(nil), base[:len(base)-2]...)
	hidden = append(hidden,
		"--tmpfs", target,
		"--remount-ro", target,
		"--remount-ro", aliasParent,
	)
	runHelperInBwrap(t, bwrap, hidden, "verify-alias-hidden", env, nil)
}

func assumeDevpts(t *testing.T, bwrap string) {
	t.Helper()
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	runHelperInBwrap(t, bwrap, args, "verify-devpts", nil, nil)
}

func assumeNetworkAndPIDIsolation(t *testing.T, bwrap string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on host loopback: %v", err)
	}
	defer func() { _ = listener.Close() }()
	env := map[string]string{
		"ASSUME_HOST_ADDR": listener.Addr().String(),
		"ASSUME_HOST_PID":  strconv.Itoa(os.Getpid()),
	}
	args := []string{
		"--die-with-parent",
		"--unshare-net",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	runHelperInBwrap(t, bwrap, args, "verify-network-pid", env, nil)
}

func assumeSealedMemfdMaterialization(t *testing.T, bwrap string) {
	t.Helper()
	memfd, err := unix.MemfdCreate(
		"tclaude-sandbox-assumption",
		unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING,
	)
	if err != nil {
		t.Fatalf("create memfd: %v", err)
	}
	file := os.NewFile(uintptr(memfd), "sandbox-assumption-image")
	if file == nil {
		t.Fatal("wrap memfd")
	}
	defer func() { _ = file.Close() }()
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatalf("open test executable: %v", err)
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = source.Close()
		t.Fatalf("copy test executable into memfd: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close test executable: %v", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind memfd: %v", err)
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		t.Fatalf("seal memfd: %v", err)
	}

	imageRoot := canonicalTempDir(t)
	stablePath := filepath.Join(imageRoot, "assumption-helper")
	cmd := exec.Command(
		bwrap,
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--tmpfs", imageRoot,
		"--perms", "0500",
		"--file", "3", stablePath,
		"--remount-ro", imageRoot,
		"--",
		stablePath,
		"-test.run=^TestBubblewrapAssumptionHelper$",
	)
	cmd.Env = append(os.Environ(),
		linuxAssumptionHelperEnv+"=verify-stable-executable",
		"ASSUME_STABLE_PATH="+stablePath,
	)
	cmd.ExtraFiles = []*os.File{file}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("run materialized sealed memfd: %v; output=%s", err, output.String())
	}
}

func assumeMissingPolicyChild(t *testing.T, bwrap string) {
	t.Helper()
	parent := canonicalTempDir(t)
	child := filepath.Join(parent, "claude-code")
	policy := filepath.Join(child, "managed-settings.json")
	payload, err := os.CreateTemp("", "tclaude-policy-assumption-*")
	if err != nil {
		t.Fatalf("create policy payload: %v", err)
	}
	defer func() {
		_ = payload.Close()
		_ = os.Remove(payload.Name())
	}()
	if _, err := payload.WriteString(`{"sandbox":{"enabled":true}}`); err != nil {
		t.Fatalf("write policy payload: %v", err)
	}
	if _, err := payload.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind policy payload: %v", err)
	}
	cmd := helperCommand(
		bwrap,
		[]string{
			"--die-with-parent",
			"--ro-bind", "/", "/",
			"--tmpfs", parent,
			"--dir", child,
			"--perms", "0400",
			"--file", "3", policy,
			"--remount-ro", parent,
		},
		"verify-policy-root",
		map[string]string{"ASSUME_POLICY_FILE": policy},
	)
	cmd.ExtraFiles = []*os.File{payload}
	runCommand(t, cmd, 10*time.Second)
}

func TestBubblewrapAssumptionHelper(t *testing.T) {
	mode := os.Getenv(linuxAssumptionHelperEnv)
	if mode == "" {
		t.Skip("bubblewrap assumption helper subprocess")
	}
	switch mode {
	case "tty-session":
		linuxHelperTTYSession(t)
	case "wait-stdin":
		_, err := io.Copy(io.Discard, os.Stdin)
		if err != nil {
			t.Fatalf("wait for stdin close: %v", err)
		}
	case "verify-remount":
		linuxHelperVerifyRemount(t)
	case "verify-private-child":
		linuxHelperVerifyPrivateChild(t)
	case "verify-alias-readable":
		data, err := os.ReadFile(os.Getenv("ASSUME_ALIAS_FILE"))
		if err != nil || string(data) != "alias-ok" {
			t.Fatalf("read materialized alias: data=%q err=%v", data, err)
		}
	case "verify-alias-hidden":
		if _, err := os.ReadFile(os.Getenv("ASSUME_ALIAS_FILE")); err == nil {
			t.Fatal("alias pierced a target-side hide")
		}
	case "verify-devpts":
		linuxHelperVerifyDevpts(t)
	case "verify-network-pid":
		linuxHelperVerifyNetworkPID(t)
	case "verify-stable-executable":
		if filepath.Clean(os.Args[0]) != filepath.Clean(os.Getenv("ASSUME_STABLE_PATH")) {
			t.Fatalf("executed path %q, want stable materialized path %q",
				os.Args[0], os.Getenv("ASSUME_STABLE_PATH"))
		}
		if _, err := os.ReadFile(os.Args[0]); err != nil {
			t.Fatalf("materialized executable vanished after bwrap consumed its fd: %v", err)
		}
	case "verify-policy-root":
		path := os.Getenv("ASSUME_POLICY_FILE")
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), `"enabled":true`) {
			t.Fatalf("read reconstructed policy: data=%q err=%v", data, err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), "new"), []byte("no"), 0o600); err == nil {
			t.Fatal("read-only reconstructed policy parent accepted a write")
		}
	default:
		t.Fatalf("unknown bubblewrap assumption helper mode %q", mode)
	}
}

func linuxHelperTTYSession(t *testing.T) {
	t.Helper()
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	size, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatalf("read initial winsize: %v", err)
	}
	_, ttyErr := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	ttyState := "present"
	if errors.Is(ttyErr, syscall.ENOTTY) {
		ttyState = "ENOTTY"
	} else if ttyErr != nil {
		t.Fatalf("inspect controlling tty: %v", ttyErr)
	}
	fmt.Printf("READY rows=%d cols=%d tty_fg=%s\n", size.Row, size.Col, ttyState)
	input := bufio.NewReader(os.Stdin)
	if _, err := input.ReadString('\n'); err != nil {
		t.Fatalf("read resize trigger: %v", err)
	}
	auto := false
	for {
		select {
		case <-winch:
			auto = true
		default:
			goto drained
		}
	}
drained:
	size, err = unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatalf("read changed winsize: %v", err)
	}
	fmt.Printf("PROBED rows=%d cols=%d auto_winch=%t\n", size.Row, size.Col, auto)
	if _, err := input.ReadString('\n'); err != nil {
		t.Fatalf("read exit trigger: %v", err)
	}
}

func linuxHelperVerifyRemount(t *testing.T) {
	t.Helper()
	parent := os.Getenv("ASSUME_PARENT")
	child := os.Getenv("ASSUME_CHILD")
	sibling := os.Getenv("ASSUME_SIBLING")
	data, err := os.ReadFile(filepath.Join(child, "seed"))
	if err != nil || string(data) != "seed" {
		t.Fatalf("child bind did not survive parent remount: data=%q err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(child, "written"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("non-recursive parent remount made child read-only: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "blocked"), []byte("no"), 0o600); err == nil {
		t.Fatal("remounted parent accepted a write")
	}
	if _, err := os.Stat(sibling); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sibling under hidden parent should be absent, got %v", err)
	}
}

func linuxHelperVerifyPrivateChild(t *testing.T) {
	t.Helper()
	parent := os.Getenv("ASSUME_PARENT")
	child := os.Getenv("ASSUME_CHILD")
	sibling := os.Getenv("ASSUME_SIBLING")
	data, err := os.ReadFile(filepath.Join(child, "own"))
	if err != nil || string(data) != "own" {
		t.Fatalf("read sole reopened child: data=%q err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(child, "new"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("sole reopened child is not writable: %v", err)
	}
	if _, err := os.Stat(sibling); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sibling leaked through private parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "blocked"), []byte("no"), 0o600); err == nil {
		t.Fatal("private parent accepted a write outside the reopened child")
	}
}

func linuxHelperVerifyDevpts(t *testing.T) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("open fresh devpts pty: %v", err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = tty.Close()
	}()
	if err := tty.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set pty deadline: %v", err)
	}
	if _, err := ptmx.Write([]byte("devpts-roundtrip\n")); err != nil {
		t.Fatalf("write pty master: %v", err)
	}
	buf := make([]byte, 64)
	n, err := tty.Read(buf)
	if err != nil {
		t.Fatalf("read pty slave: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "devpts-roundtrip") {
		t.Fatalf("unexpected pty round-trip payload %q", buf[:n])
	}
}

func linuxHelperVerifyNetworkPID(t *testing.T) {
	t.Helper()
	hostAddr := os.Getenv("ASSUME_HOST_ADDR")
	if conn, err := net.DialTimeout("tcp4", hostAddr, 300*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("isolated namespace reached host loopback")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on namespace-local loopback: %v", err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
		accepted <- acceptErr
	}()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial namespace-local loopback: %v", err)
	}
	_ = conn.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("accept namespace-local loopback: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("namespace-local loopback round-trip timed out")
	}
	hostPID, err := strconv.Atoi(os.Getenv("ASSUME_HOST_PID"))
	if err != nil {
		t.Fatalf("parse host pid: %v", err)
	}
	if hostPID != os.Getpid() {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(hostPID))); err == nil {
			t.Fatalf("host pid %d remained visible inside isolated pid namespace", hostPID)
		}
	}
}

func helperCommand(
	bwrap string,
	args []string,
	mode string,
	env map[string]string,
) *exec.Cmd {
	argv := append([]string(nil), args...)
	argv = append(argv,
		"--",
		os.Args[0],
		"-test.run=^TestBubblewrapAssumptionHelper$",
	)
	cmd := exec.Command(bwrap, argv...)
	cmd.Env = helperEnv(mode, env)
	return cmd
}

func runHelperInBwrap(
	t *testing.T,
	bwrap string,
	args []string,
	mode string,
	env map[string]string,
	extraFiles []*os.File,
) {
	t.Helper()
	cmd := helperCommand(bwrap, args, mode, env)
	cmd.ExtraFiles = extraFiles
	runCommand(t, cmd, 10*time.Second)
}

func helperEnv(mode string, values map[string]string) []string {
	env := append(os.Environ(), linuxAssumptionHelperEnv+"="+mode)
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func runBwrap(t *testing.T, binary string, args []string, env []string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	if env != nil {
		cmd.Env = env
	}
	runCommand(t, cmd, 15*time.Second)
}

func waitCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s failed: %v", cmd.Path, err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%s timed out after %s", cmd.Path, timeout)
	}
}

func readLineWithDeadline(
	t *testing.T,
	reader *bufio.Reader,
	file *os.File,
	prefix string,
) string {
	t.Helper()
	if err := file.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set pty read deadline: %v", err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %s line: %v", prefix, err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
}
