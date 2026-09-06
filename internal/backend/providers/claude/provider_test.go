//go:build linux || darwin

package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type testPermit struct {
	execution model.ExecutionID
	operation model.OperationID
	consumed  atomic.Bool
}

func (p *testPermit) ExecutionID() model.ExecutionID { return p.execution }
func (p *testPermit) OperationID() model.OperationID { return p.operation }
func (p *testPermit) Consume(context.Context) error {
	if !p.consumed.CompareAndSwap(false, true) {
		return os.ErrPermission
	}
	return nil
}

func TestProviderOwnsTerminalLaunchInteractionRecoveryAndStop(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tclaude-claude-provider-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	argvPath := filepath.Join(root, "argv")
	inputPath := filepath.Join(root, "input")
	executable := filepath.Join(root, "claude-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CLAUDE_TEST_ARGV\"\nwhile IFS= read -r line; do\n  printf '%s\\n' \"$line\" >> \"$CLAUDE_TEST_INPUT\"\ndone\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	require.NoError(t, os.Setenv("CLAUDE_TEST_ARGV", argvPath))
	require.NoError(t, os.Setenv("CLAUDE_TEST_INPUT", inputPath))
	t.Cleanup(func() {
		_ = os.Unsetenv("CLAUDE_TEST_ARGV")
		_ = os.Unsetenv("CLAUDE_TEST_INPUT")
	})

	provider, err := New(Config{Executable: executable, PrivateRoot: root})
	require.NoError(t, err)
	request := ports.PreparationRequest{
		Intent: ports.StartFresh,
		Spec: model.ResolvedExecutionSpec{
			ExecutionID: "execution_claude", Harness: Name, Model: "test-model",
			WorkingDirectory: root, Approval: model.ApprovalSupervised,
			Sandbox: model.SandboxWorkspaceWrite,
		},
	}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	description := prepared.Describe()
	require.Equal(t, ports.TopologyTerminalAuthoritative, description.Topology)
	require.True(t, description.EffectivePolicy.SandboxEnforced)
	require.Len(t, description.Resources, 1)

	permit := &testPermit{execution: request.Spec.ExecutionID, operation: "operation_launch"}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	require.True(t, permit.consumed.Load())
	require.Equal(t, ports.ReleaseStarted, released.State)
	require.NotNil(t, released.Runtime)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})

	preparedRecovery, err := provider.Recover(context.Background(), ports.RecoveryRequest{
		ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: description.Evidence,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, preparedRecovery.State,
		"the evidence persisted before release must recover a start completed before receipt persistence")

	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(argvPath)
		return readErr == nil && strings.Contains(string(value), "--session-id") &&
			strings.Contains(string(value), "--permission-mode\nmanual")
	}, time.Second, 10*time.Millisecond)

	interaction, err := released.Runtime.Interact(context.Background(), ports.Interaction{Text: "literal $(touch nope); `false`"})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, interaction.Disposition)
	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(inputPath)
		return readErr == nil && strings.Contains(string(value), "literal $(touch nope); `false`")
	}, time.Second, 10*time.Millisecond)
	require.NoFileExists(t, filepath.Join(root, "nope"))

	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{
		ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: released.Evidence,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	observation, err := recovered.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadRunning, observation.Workload)
	require.Equal(t, ports.ContextUnknown, observation.Context,
		"terminal liveness must not be promoted to harness readiness")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped, err := recovered.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.True(t, stopped.Acknowledged)
}

func TestProviderRefusesUnsupportedReadOnlyConfinement(t *testing.T) {
	root := t.TempDir()
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root})
	require.NoError(t, err)
	_, err = provider.Prepare(context.Background(), ports.PreparationRequest{
		Intent: ports.StartFresh,
		Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_refused", Harness: Name,
			WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxReadOnly},
	})
	require.ErrorContains(t, err, "does not enforce")
}
