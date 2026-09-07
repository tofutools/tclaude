//go:build linux || darwin

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
)

func TestPublicCheckoutShellSurvivesBackendRestart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tcl-sj-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.Mkdir(repo, 0700))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-b", "main")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "base")
	state := filepath.Join(root, "state")
	require.NoError(t, Initialize(state))
	token, err := os.ReadFile(filepath.Join(state, "operator.token"))
	require.NoError(t, err)
	checkout, err := host.NewCheckoutHost("")
	require.NoError(t, err)
	shells, err := host.NewShellHost(host.ShellConfig{Terminal: host.TerminalHost{PrivateRoot: filepath.Join(root, "term")}, Executable: "/bin/sh"})
	require.NoError(t, err)
	services := JourneyServices{Workspaces: checkout, Shells: shells}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(state, "api.sock"))
	}}}
	defer client.CloseIdleConnections()
	call := func(method, path string, body any, out any) {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest(method, "http://backend"+path, bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+string(token))
		res, err := client.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		payload, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Less(t, res.StatusCode, 300, "%s: %s", path, payload)
		if out != nil {
			require.NoError(t, json.Unmarshal(payload, out), "%s", payload)
		}
	}
	start := func() func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, state, providers.NewRegistry(), services) }()
		stop := func() { cancel(); require.NoError(t, <-done); client.CloseIdleConnections() }
		// Schema initialization and recovery run before listen; allow the same
		// bounded startup budget under a contended race-suite runner.
		deadline := time.Now().Add(20 * time.Second)
		for {
			select {
			case err := <-done:
				cancel()
				t.Fatalf("server exited before readiness: %v", err)
			default:
			}
			req, _ := http.NewRequest("GET", "http://backend/v2/snapshot", nil)
			req.Header.Set("Authorization", "Bearer "+string(token))
			res, err := client.Do(req)
			if err == nil {
				res.Body.Close()
				if res.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				cancel()
				serverErr := <-done
				client.CloseIdleConnections()
				t.Fatalf("server unavailable after startup budget: probe=%v server=%v", err, serverErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return stop
	}
	stop := start()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	path := filepath.Join(root, "checkout")
	var workspace app.WorkspaceResult
	call("POST", "/v2/workspaces/create", map[string]any{"request_id": "create", "id": "workspace_public", "intent": model.WorkspaceIntent{Repository: repo, IntendedPath: path, BaseRevision: "main", Branch: "worker", RetainOnFinish: true}}, &workspace)
	require.Equal(t, model.WorkspaceAvailable, workspace.Workspace.State)
	var launch struct {
		Execution struct {
			ID       model.ExecutionID `json:"id"`
			Workload string            `json:"workload"`
		} `json:"execution"`
	}
	call("POST", "/v2/shells", map[string]any{"request_id": "shell", "workspace_id": workspace.Workspace.ID, "expected_revision": workspace.Workspace.Revision, "sandbox": "unconfined"}, &launch)
	require.NotEmpty(t, launch.Execution.ID)
	require.Equal(t, "shell", launch.Execution.Workload)
	defer func() {
		if stop != nil {
			call("POST", "/v2/stop", map[string]any{"request_id": "cleanup", "execution_id": launch.Execution.ID, "force": true}, nil)
		}
	}()
	stop()
	stop = nil
	stop = start()
	var observation struct {
		Workload string `json:"workload"`
	}
	call("POST", "/v2/observe", map[string]any{"execution_id": launch.Execution.ID}, &observation)
	require.Equal(t, "running", observation.Workload)
	var snapshot struct {
		Agents        []json.RawMessage `json:"agents"`
		Conversations []json.RawMessage `json:"conversations"`
	}
	call("GET", "/v2/snapshot", nil, &snapshot)
	require.Empty(t, snapshot.Agents)
	require.Empty(t, snapshot.Conversations)
	call("POST", "/v2/stop", map[string]any{"request_id": "stop", "execution_id": launch.Execution.ID, "force": true}, nil)
	require.DirExists(t, path, "stopping a shell must retain its checkout")
	// Stop acceptance can precede process exit. Observe the terminal state before
	// asking removal to release an actively claimed checkout.
	require.Eventually(t, func() bool {
		call("POST", "/v2/observe", map[string]any{"execution_id": launch.Execution.ID}, &observation)
		return observation.Workload == "exited"
	}, 5*time.Second, 20*time.Millisecond)
	call("GET", "/v2/workspaces/workspace_public", nil, &workspace)
	call("POST", "/v2/workspaces/remove", map[string]any{"request_id": "remove", "workspace_id": workspace.Workspace.ID, "expected_revision": workspace.Workspace.Revision, "destructive": false}, nil)
	require.NoDirExists(t, path)
}
