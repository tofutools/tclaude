//go:build linux || darwin

package host

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestShellHostLaunchesRecoversAndStopsOwnedTerminal(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tclaude-shell-host-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	host, err := NewShellHost(ShellConfig{Terminal: TerminalHost{PrivateRoot: root}, Executable: "/bin/sh"})
	require.NoError(t, err)
	request := ports.ShellPreparationRequest{ExecutionID: "execution_shell", Attempt: 1,
		WorkspaceID: "workspace_shell", WorkingDirectory: root, Sandbox: model.SandboxUnconfined}
	prepared, err := host.PrepareShell(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, request.ExecutionID, prepared.Describe().ExecutionID)
	permit := &shellPermit{execution: request.ExecutionID, operation: "operation_shell"}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	require.True(t, permit.consumed)
	require.Equal(t, ports.ReleaseStarted, released.State)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.StopHost(ctx, ports.StopRequest{Force: true})
	})
	require.Eventually(t, func() bool {
		observation, _ := released.Runtime.ObserveHost(context.Background())
		return observation.Workload == ports.WorkloadRunning
	}, time.Second, 10*time.Millisecond)
	recovered, err := host.RecoverShell(context.Background(), ports.ShellRecoveryRequest{
		ExecutionID: request.ExecutionID, Attempt: request.Attempt, WorkspaceID: request.WorkspaceID, Evidence: released.Evidence,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	stopped, err := recovered.Runtime.StopHost(context.Background(), ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.True(t, stopped.Acknowledged)
}

func TestShellHostReportsUnsupportedConfinementWithoutPreparing(t *testing.T) {
	host, err := NewShellHost(ShellConfig{Terminal: TerminalHost{PrivateRoot: t.TempDir()}, Executable: "/bin/sh"})
	require.NoError(t, err)
	_, err = host.PrepareShell(context.Background(), ports.ShellPreparationRequest{
		ExecutionID: "execution_shell", Attempt: 1, WorkspaceID: "workspace_shell",
		WorkingDirectory: t.TempDir(), Sandbox: model.SandboxWorkspaceWrite,
	})
	require.ErrorContains(t, err, "does not provide OS confinement")
}

type shellPermit struct {
	execution model.ExecutionID
	operation model.OperationID
	consumed  bool
}

func (p *shellPermit) ExecutionID() model.ExecutionID { return p.execution }
func (p *shellPermit) OperationID() model.OperationID { return p.operation }
func (p *shellPermit) Consume(context.Context) error {
	p.consumed = true
	return nil
}
