package agentd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const serveCollabOrderHelperEnv = "TCLAUDE_AGENTD_SERVE_COLLAB_ORDER_SOCKET"

// TestServeCollabOrderHelper is the daemon half of
// TestServeConfigWriteRightAfterStartSurvives: a real runServe whose Collab
// default store holds its read of config.json open for a second.
func TestServeCollabOrderHelper(t *testing.T) {
	socket := os.Getenv(serveCollabOrderHelperEnv)
	if socket == "" {
		t.Skip("helper for TestServeConfigWriteRightAfterStartSurvives")
	}
	collabDefaultDecideHookForTest = func() { time.Sleep(time.Second) }
	require.NoError(t, runServe(&serveParams{
		Socket: socket, NoTray: true, NoPrintHumanToken: true,
	}))
}

// A client that writes config.json as soon as the daemon's socket answers
// (tclaude config set, the dashboard Config form, the setup wizard) must not
// lose that write to the daemon's first-start Collab default store, which
// read-modify-writes the same file. The daemon stores it before binding the
// socket, so no client can reach it while the store is in flight. The store
// is held open for a second here; were it after the bind, the write below
// would land inside that window and be replaced every time.
func TestServeConfigWriteRightAfterStartSurvives(t *testing.T) {
	home := filepath.Join(testutil.CanonicalTempDir(t), "home")
	require.NoError(t, os.MkdirAll(home, 0o700))
	// Unix socket paths are short on macOS; keep this one out of TMPDIR.
	socketDir, err := os.MkdirTemp("/tmp", "tclaude-collab-order-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "agentd.sock")

	environment := map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, "config"),
		"XDG_DATA_HOME":   filepath.Join(home, "data"),
		"XDG_CACHE_HOME":  filepath.Join(home, "cache"),
		"XDG_STATE_HOME":  filepath.Join(home, "state"),
	}
	for name, value := range environment {
		t.Setenv(name, value)
	}
	t.Setenv(agentipc.SocketEnv, socket)

	daemon := exec.Command(os.Args[0],
		"-test.run=^TestServeCollabOrderHelper$", "-test.count=1", "-test.v")
	// A clean daemon environment: an ambient tclaude session (an agent running
	// these tests) carries its own resource delegation and tmux pointers.
	var daemonEnv []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "TCLAUDE_RESOURCE_DELEGATION_DIR" || name == "TMUX" || name == "TMUX_PANE" {
			continue
		}
		daemonEnv = append(daemonEnv, entry)
	}
	daemon.Env = append(daemonEnv, serveCollabOrderHelperEnv+"="+socket)
	var output bytes.Buffer
	daemon.Stdout = &output
	daemon.Stderr = &output
	require.NoError(t, daemon.Start())
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	t.Cleanup(func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			_ = daemon.Process.Kill()
			<-exited
		}
		if t.Failed() {
			t.Logf("daemon output:\n%s", output.String())
		}
	})

	reachable := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		if agentipc.SocketReachable(socket) {
			reachable = true
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("daemon exited before serving: %v\n%s", err, output.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	require.True(t, reachable, "daemon never served its socket")

	// The client's write, the instant the daemon answers.
	_, err = config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		cfg.LogLevel = "debug"
		return nil
	})
	require.NoError(t, err)

	// Outlast any store that could still be in flight, then read the file.
	time.Sleep(2 * time.Second)
	stored, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "debug", stored.LogLevel,
		"the daemon's Collab default store replaced a config write made after its socket answered")
	require.NotNil(t, stored.Features)
	require.NotNil(t, stored.Features.Collab, "the daemon stored its Collab default")
}
