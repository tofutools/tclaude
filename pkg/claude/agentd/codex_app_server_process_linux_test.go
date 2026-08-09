//go:build linux

package agentd

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexAppServerArgsTargetExactGenerationSocket(t *testing.T) {
	args := [][]byte{[]byte("/opt/tclaude"), []byte("session"), []byte("codex-app-server-relay"), []byte("--socket"), []byte("/private/generation/app.sock"), []byte("--upstream"), []byte("127.0.0.1:1234")}
	assert.True(t, codexAppServerArgsTargetSocket(args, "/private/generation/app.sock"))
	assert.False(t, codexAppServerArgsTargetSocket(args, "/private/replacement/app.sock"))
	assert.False(t, codexAppServerArgsTargetSocket(
		[][]byte{[]byte("/opt/tclaude"), []byte("session"), []byte("codex-app-server-relay"), []byte("--socket"), []byte("/private/generation/app.sock.extra")},
		"/private/generation/app.sock"), "socket argv proof must be an exact argument match")
}

func TestCodexServerArgsTargetExactAuthenticatedEndpoint(t *testing.T) {
	// The current test process does not carry a server argv, so exercise the
	// exact argument matcher through a short-lived helper process.
	if os.Getenv("TCLAUDE_CODEX_ARGV_HELPER") == "1" {
		select {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestCodexServerArgsTargetExactAuthenticatedEndpoint$", "--",
		"app-server", "--listen", "ws://127.0.0.1:43210")
	cmd.Env = append(os.Environ(), "TCLAUDE_CODEX_ARGV_HELPER=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(time.Second)
	for !codexServerArgsTargetEndpoint(cmd.Process.Pid, "ws://127.0.0.1:43210") {
		if time.Now().After(deadline) {
			t.Fatal("helper argv was not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.False(t, codexServerArgsTargetEndpoint(cmd.Process.Pid, "ws://127.0.0.1:43211"))
	_, err := readCodexAppServerProcessIdentity(cmd.Process.Pid, "/private/generation/app.sock")
	assert.Error(t, err, "the native server process must never satisfy the relay identity proof")
}

func TestCodexRelayOwnsExactAuthenticatedTUIEndpoint(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_RELAY_ARGV_HELPER") == "1" {
		tcpListener, err := net.Listen("tcp4", strings.TrimPrefix(
			os.Getenv("TCLAUDE_CODEX_RELAY_ENDPOINT"), "ws://"))
		require.NoError(t, err)
		defer tcpListener.Close()
		require.NoError(t, os.WriteFile(os.Getenv("TCLAUDE_CODEX_RELAY_READY"), []byte("ready"), 0o600))
		select {}
	}
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "app.sock")
	readyPath := filepath.Join(dir, "ready")
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := "ws://" + reservation.Addr().String()
	require.NoError(t, reservation.Close())
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestCodexRelayOwnsExactAuthenticatedTUIEndpoint$", "--",
		"codex-app-server-relay", "--socket", socketPath, "--listen", endpoint)
	cmd.Env = append(os.Environ(),
		"TCLAUDE_CODEX_RELAY_ARGV_HELPER=1",
		"TCLAUDE_CODEX_RELAY_SOCKET="+socketPath,
		"TCLAUDE_CODEX_RELAY_ENDPOINT="+endpoint,
		"TCLAUDE_CODEX_RELAY_READY="+readyPath)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(readyPath)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	assert.True(t, processOwnsCodexAppServerRelayEndpoint(
		cmd.Process.Pid, socketPath, endpoint))
	assert.False(t, processOwnsCodexAppServerRelayEndpoint(
		cmd.Process.Pid, socketPath, "ws://127.0.0.1:1"))
	assert.False(t, processOwnsCodexAppServerEndpoint(cmd.Process.Pid, endpoint),
		"the relay listener must never satisfy the native app-server ownership proof")
}

func TestCodexAppServerDiscoversHostPIDsAcrossBwrapPIDAndNetworkNamespaces(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_NAMESPACE_HELPER") == "1" {
		socketPath := os.Getenv("TCLAUDE_CODEX_NAMESPACE_SOCKET")
		endpoint := os.Getenv("TCLAUDE_CODEX_NAMESPACE_ENDPOINT")
		unixListener, err := net.Listen("unix", socketPath)
		require.NoError(t, err)
		defer unixListener.Close()
		tcpListener, err := net.Listen("tcp4", strings.TrimPrefix(endpoint, "ws://"))
		require.NoError(t, err)
		defer tcpListener.Close()
		require.NoError(t, os.WriteFile(os.Getenv("TCLAUDE_CODEX_NAMESPACE_READY"), []byte("ready"), 0o600))
		select {}
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is unavailable")
	}
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-namespace-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socketPath := filepath.Join(dir, "app.sock")
	readyPath := filepath.Join(dir, "ready")
	endpoint := "ws://127.0.0.1:43210"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrap,
		"--die-with-parent", "--ro-bind", "/", "/", "--bind", dir, dir,
		"--unshare-net", "--unshare-pid", "--proc", "/proc", os.Args[0],
		"-test.run=^TestCodexAppServerDiscoversHostPIDsAcrossBwrapPIDAndNetworkNamespaces$", "--",
		"codex-app-server-relay", "--socket", socketPath,
		"app-server", "--listen", endpoint)
	cmd.Env = append(os.Environ(),
		"TCLAUDE_CODEX_NAMESPACE_HELPER=1",
		"TCLAUDE_CODEX_NAMESPACE_SOCKET="+socketPath,
		"TCLAUDE_CODEX_NAMESPACE_ENDPOINT="+endpoint,
		"TCLAUDE_CODEX_NAMESPACE_READY="+readyPath)
	if err := cmd.Start(); err != nil {
		t.Skipf("bubblewrap namespace probe unavailable: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			_ = cmd.Wait()
			t.Skip("bubblewrap namespace probe could not start in this environment")
		}
		time.Sleep(25 * time.Millisecond)
	}
	proofCtx, proofCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer proofCancel()
	relayPID, err := discoverCodexAppServerRelayPID(proofCtx, socketPath)
	require.NoError(t, err)
	require.Greater(t, relayPID, 1)
	serverPID, err := discoverCodexAppServerPID(proofCtx, socketPath, filepath.Join(dir, "unused.pid"))
	require.NoError(t, err)
	assert.Equal(t, relayPID, serverPID)
	assert.True(t, processOwnsCodexAppServerEndpoint(serverPID, endpoint))
	_, err = readCodexAppServerProcessIdentity(relayPID, socketPath)
	require.NoError(t, err)
}

func TestCodexAppServerSocketOwnerDiscoveryUsesTargetNamespaceTables(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tcl-codex-owner-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	socketPath := filepath.Join(dir, "app.sock")
	unixListener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unixListener.Close()) })

	inode, err := findUnixSocketInodeAcrossNamespaces(socketPath)
	require.NoError(t, err)
	assert.True(t, processHoldsSocket(os.Getpid(), inode))
	assert.Equal(t, os.Getpid(), findProcessHoldingSocket(inode))

	tcpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tcpListener.Close()) })
	port := tcpListener.Addr().(*net.TCPAddr).Port
	tcpInode, err := findTCPListenerInode(
		filepath.Join("/proc", strconv.Itoa(os.Getpid()), "net", "tcp"), port)
	require.NoError(t, err)
	assert.True(t, processHoldsSocket(os.Getpid(), tcpInode))
	assert.False(t, processOwnsCodexAppServerEndpoint(os.Getpid(),
		"ws://127.0.0.1:"+strconv.Itoa(port)),
		"a process that owns the port but lacks the exact native server argv must be rejected")
}
