package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/migration"
	_ "modernc.org/sqlite"
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
	// Import a real frozen-schema offline bundle through the shipped CLI, verify
	// exact retry before first serve, then inspect retained meaning publicly.
	bundle := filepath.Join(root, "bundle")
	require.NoError(t, os.Mkdir(bundle, 0700))
	schema, err := os.ReadFile("internal/backend/migration/source/v228/testdata/schema.sql")
	require.NoError(t, err)
	sourcePath := filepath.Join(bundle, "snapshot.sqlite")
	source, err := sql.Open("sqlite", sourcePath)
	require.NoError(t, err)
	_, err = source.Exec(string(schema))
	require.NoError(t, err)
	_, err = source.Exec(`INSERT INTO schema_version(version) VALUES(228);
 INSERT INTO agents(agent_id,current_conv_id,created_at,pending_name,task_ref_url) VALUES('old-worker','old-native',1,'Imported worker','https://tracker.invalid/retained');
 INSERT INTO agent_conversations(conv_id,agent_id,linked_at) VALUES('old-native','old-worker',1);
 INSERT INTO conv_index(conv_id,project_dir,full_path,custom_title,harness) VALUES('old-native','/old/project','/old/native','Imported worker','codex');
 INSERT INTO human_messages(from_conv,from_agent,body,created_at) VALUES('old-native','old-worker','Retained correspondence',1);`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	sourceBytes, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	sum := sha256.Sum256(sourceBytes)
	manifest := migration.Manifest{FormatVersion: 1, Database: migration.ManifestFile{Path: "snapshot.sqlite", Size: int64(len(sourceBytes)), SHA256: hex.EncodeToString(sum[:])}}
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), manifestBytes, 0600))
	state = filepath.Join(root, "imported")
	importArgs := []string{"migration", "import", "--bundle", bundle, "--manifest", "manifest.json", "--state-dir", state}
	out, err = run(cli, importArgs...)
	require.NoError(t, err, string(out))
	out, err = run(cli, importArgs...)
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), `"Repeated": true`)
	out, err = run(cli, "migration", "report", "--state-dir", state)
	require.NoError(t, err, string(out))
	require.NotContains(t, string(out), "Retained correspondence")
	stop = start(daemon, "serve", "--state-dir", state)
	out, err = run(cli, "--operator-state", state, "snapshot")
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "Imported worker")
	require.Contains(t, string(out), "Retained correspondence")
	require.Contains(t, string(out), "https://tracker.invalid/retained")
	stop()
	after, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	require.Equal(t, sourceBytes, after)

}
