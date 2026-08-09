package codexappserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Run with TCLAUDE_CODEX_APPSERVER_LIVE=1. The normal suite requires neither
// an installed Codex binary nor authentication.
func TestLiveCodexAppServerHandshake(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_APPSERVER_LIVE") != "1" {
		t.Skip("set TCLAUDE_CODEX_APPSERVER_LIVE=1 to run against installed Codex")
	}
	versionOutput, err := exec.Command("codex", "--version").Output()
	require.NoError(t, err)
	version := strings.TrimSpace(strings.TrimPrefix(string(versionOutput), "codex-cli "))
	require.NoError(t, codexappserver.CheckVersion(version))

	runtimeDir, err := os.MkdirTemp("/tmp", "codexappserver-live-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(runtimeDir)) })
	codexHome := filepath.Join(runtimeDir, "home")
	require.NoError(t, os.Mkdir(codexHome, 0o700))
	socketPath := filepath.Join(runtimeDir, "app.sock")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	upstream := listener.Addr().String()
	require.NoError(t, listener.Close())
	const token = "real-codex-generation-capability"
	digest := sha256.Sum256([]byte(token))

	processCtx, stopProcess := context.WithCancel(context.Background())
	var output bytes.Buffer
	command := exec.CommandContext(processCtx, "codex", "app-server",
		"--listen", "ws://"+upstream,
		"--ws-auth", "capability-token",
		"--ws-token-sha256", hex.EncodeToString(digest[:]))
	command.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	command.Stdout = &output
	command.Stderr = &output
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		stopProcess()
		_ = command.Wait()
	})
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	go func() { relayDone <- session.RunCodexAppServerRelay(relayCtx, socketPath, upstream) }()
	t.Cleanup(func() {
		stopRelay()
		require.NoError(t, <-relayDone)
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, statErr := os.Lstat(socketPath); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Codex app-server socket did not appear: %s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	for {
		probe, dialErr := net.DialTimeout("tcp", upstream, 100*time.Millisecond)
		if dialErr == nil {
			_ = probe.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("authenticated Codex app-server did not listen: %s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	unauthorized, unauthorizedErr := codexappserver.Dial(ctx, socketPath,
		&codexappserver.Options{CodexVersion: version})
	require.Error(t, unauthorizedErr, "known relay endpoint must not authorize a client")
	require.Nil(t, unauthorized)
	client, err := codexappserver.Dial(ctx, socketPath, &codexappserver.Options{
		CodexVersion: version, BearerToken: token,
	})
	require.NoError(t, err, output.String())
	defer client.Close()
	_, err = client.ListLoadedThreads(ctx, codexappserver.ThreadLoadedListParams{})
	require.NoError(t, err)
}
