package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the shipped binaries with a disposable state directory. No provider
// is enabled: durable operator work must not depend on a native installation.
func TestShippedProductEntrypointsRetainStateAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "tcl-entry-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TCLAUDE_BACKEND_SOCKET=") && !strings.HasPrefix(e, "TCLAUDE_BACKEND_CREDENTIAL_FILE=") {
			env = append(env, e)
		}
	}
	cli, daemon := filepath.Join(root, "tclaude"), filepath.Join(root, "tclaude-agentd")
	run := func(binary string, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, binary, args...)
		c.Env = env
		return c.CombinedOutput()
	}
	for _, b := range []struct{ out, pkg string }{{cli, "."}, {daemon, "./cmd/tclaude-agentd"}} {
		out, err := run("go", "build", "-o", b.out, b.pkg)
		require.NoError(t, err, string(out))
	}
	for _, b := range []string{cli, daemon} {
		out, err := run(b, "--version")
		require.NoError(t, err, string(out))
		require.Contains(t, string(out), "development")
	}
	state := filepath.Join(root, "state")
	out, err := run(cli, "agentd", "--state-dir", state, "--init")
	require.NoError(t, err, string(out))
	out, err = run(daemon, "--state-dir", state, "--init")
	require.Error(t, err, "initialization must not adopt existing state: %s", out)
	start := func(binary string, args ...string) func() {
		log, err := os.CreateTemp(root, "daemon-log-")
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		cmd.Stdout = log
		cmd.Stderr = log
		require.NoError(t, cmd.Start())
		done := make(chan error, 1)
		go func() { done <- cmd.Wait(); close(done) }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("daemon did not join shutdown")
			}
			require.NoError(t, log.Close())
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(state, "api.sock")); err == nil {
				return stop
			}
			select {
			case err := <-done:
				data, _ := os.ReadFile(log.Name())
				t.Fatalf("daemon exited before readiness: %v %s", err, data)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("daemon socket not ready")
		return stop
	}
	stop := start(daemon, "serve", "--state-dir", state)
	input := filepath.Join(root, "agent.json")
	data, err := json.Marshal(map[string]any{"id": "retained-worker", "name": "Retained worker", "desired": map[string]any{"Harness": "claude", "Model": "fixture", "WorkingDirectory": root, "Approval": "supervised", "Sandbox": "unconfined"}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input, data, 0600))
	out, err = run(cli, "--operator-state", state, "agent", "create", "--file", input)
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "retained-worker")
	stop()
	stop = start(cli, "agentd", "serve", "--state-dir", state)
	out, err = run(cli, "--operator-state", state, "snapshot")
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "Retained worker")
	stop()
}
