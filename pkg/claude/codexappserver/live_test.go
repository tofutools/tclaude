package codexappserver_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
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

	processCtx, stopProcess := context.WithCancel(context.Background())
	var output bytes.Buffer
	command := exec.CommandContext(processCtx, "codex", "app-server", "--listen", "unix://"+socketPath)
	command.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	command.Stdout = &output
	command.Stderr = &output
	require.NoError(t, command.Start())
	t.Cleanup(func() {
		stopProcess()
		_ = command.Wait()
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := codexappserver.Dial(ctx, socketPath, &codexappserver.Options{CodexVersion: version})
	require.NoError(t, err, output.String())
	defer client.Close()
	_, err = client.ListLoadedThreads(ctx, codexappserver.ThreadLoadedListParams{})
	require.NoError(t, err)
}
