//go:build linux || darwin

package copilot_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestApplicationSQLiteTerminalFlowWithNativeDouble(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tcl-copilot-app-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	input := filepath.Join(root, "input")
	executable := filepath.Join(root, "copilot-native-double")
	script := "#!/bin/sh\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> \"" + input + "\"; done\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	provider, err := copilot.New(copilot.Config{Executable: executable, PrivateRoot: filepath.Join(root, "provider"), AgentSocket: filepath.Join(root, "agent.sock")})
	require.NoError(t, err)
	store, err := backendsqlite.Open(filepath.Join(root, "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry(provider)).WithAgentAPIEndpoint(filepath.Join(root, "agent.sock"))
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: copilot.Name, Model: "test-model", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	launched, err := service.Launch(context.Background(), app.LaunchRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "request_launch"}, Target: app.LaunchTarget{Standalone: &app.StandaloneLaunchTarget{Desired: desired}}})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionRunning, launched.Execution.State)
	_, err = service.Interact(context.Background(), app.InteractRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "request_interact"}, ExecutionID: launched.Execution.ID, Text: "hello from app"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(input)
		return readErr == nil && string(raw) == "hello from app\n"
	}, time.Second, 10*time.Millisecond)
	attached, err := service.Attach(context.Background(), app.AttachRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "request_attach"}, ExecutionID: launched.Execution.ID, Kind: ports.AttachmentTerminal})
	require.NoError(t, err)
	require.NoError(t, attached.Attachment.Close())
	snapshot, err := service.Snapshot(context.Background(), app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Executions, 1)
	require.Len(t, snapshot.Operations, 3)
	stopped, err := service.Stop(context.Background(), app.StopRequest{RequestContext: app.RequestContext{Principal: operator, RequestID: "request_stop"}, ExecutionID: launched.Execution.ID, Force: true})
	require.NoError(t, err)
	require.Equal(t, model.ExecutionExited, stopped.Execution.State)
}
