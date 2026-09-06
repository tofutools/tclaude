//go:build linux || darwin

package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type testPermit struct {
	execution model.ExecutionID
	consumed  atomic.Bool
}

func (p *testPermit) ExecutionID() model.ExecutionID { return p.execution }
func (*testPermit) OperationID() model.OperationID   { return "operation_launch" }
func (p *testPermit) Consume(context.Context) error {
	if !p.consumed.CompareAndSwap(false, true) {
		return os.ErrPermission
	}
	return nil
}

type observationSink struct {
	values []ports.PrimaryContextEvidence
}

func (s *observationSink) ObservePrimaryContext(_ context.Context, value ports.PrimaryContextEvidence) error {
	s.values = append(s.values, value)
	return nil
}

func TestProviderOwnsTerminalCredentialAndRecovery(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tcl-copilot-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	argvPath := filepath.Join(root, "argv")
	inputPath := filepath.Join(root, "input")
	bootstrapPath := filepath.Join(root, "bootstrap")
	executable := filepath.Join(root, "copilot-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$COPILOT_TEST_ARGV\"\nprintf '%s\\n' \"$COPILOT_HOME\" \"$TCLAUDE_BACKEND_SOCKET\" > \"$COPILOT_TEST_BOOTSTRAP\"\ncat \"$TCLAUDE_BACKEND_CREDENTIAL_FILE\" >> \"$COPILOT_TEST_BOOTSTRAP\"\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> \"$COPILOT_TEST_INPUT\"; done\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	t.Setenv("COPILOT_TEST_ARGV", argvPath)
	t.Setenv("COPILOT_TEST_INPUT", inputPath)
	t.Setenv("COPILOT_TEST_BOOTSTRAP", bootstrapPath)
	provider, err := New(Config{Executable: executable, PrivateRoot: root, AgentSocket: filepath.Join(root, "agent.sock")})
	require.NoError(t, err)
	expires := time.Now().Add(time.Hour)
	request := ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_copilot", Attempt: 3, Harness: Name, Model: "test-model", WorkingDirectory: root, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxUnconfined}, ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "execution_copilot", Generation: 1, DeliveryID: "delivery", Secret: []byte("copilot-secret"), ExpiresAt: expires}}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	description := prepared.Describe()
	require.Equal(t, ports.TopologyTerminalAuthoritative, description.Topology)
	require.Equal(t, []model.SandboxMode{model.SandboxUnconfined}, description.Requirements.Policy.SupportedSandbox)
	require.NotContains(t, string(description.Evidence.Payload), "copilot-secret")
	permit := &testPermit{execution: request.Spec.ExecutionID}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(argvPath)
		return readErr == nil && strings.Contains(string(raw), "--allow-all-tools") && strings.Contains(string(raw), "--no-ask-user")
	}, time.Second, 10*time.Millisecond)
	interaction, err := released.Runtime.Interact(context.Background(), ports.Interaction{Text: "literal $(touch nope); `false`"})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, interaction.Disposition)
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(inputPath)
		return readErr == nil && strings.Contains(string(raw), "literal $(touch nope); `false`")
	}, time.Second, 10*time.Millisecond)
	require.NoFileExists(t, filepath.Join(root, "nope"))
	access := &model.ExecutionAccessBinding{ExecutionID: request.Spec.ExecutionID, Generation: 1, DeliveryID: "delivery", State: model.ExecutionAccessSuspended, ExpiresAt: expires}
	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: released.Evidence, Attempt: request.Spec.Attempt, Access: access})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	require.NotNil(t, recovered.AccessProof)
}

func TestProviderRejectsRequestedConfinementInsteadOfFallingBack(t *testing.T) {
	root := t.TempDir()
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root})
	require.NoError(t, err)
	_, err = provider.Prepare(context.Background(), ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{ExecutionID: "execution", Harness: Name, WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.ErrorContains(t, err, "does not enforce platform mode")
}

func TestSessionStartObservationRequiresExpectedPrimarySession(t *testing.T) {
	spool, err := host.PrepareObservationSpool(filepath.Join(t.TempDir(), "observations"))
	require.NoError(t, err)
	sink := &observationSink{}
	runtime := &Runtime{executionID: "execution", attempt: 2, nativeID: "00000000-0000-4000-8000-000000000001", intent: ports.StartFresh, observations: sink, spool: spool}
	writeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: "00000000-0000-4000-8000-000000000002", HookEventName: "sessionStart", Source: "startup"})
	require.NoError(t, runtime.consumeObservations(context.Background()))
	require.Empty(t, sink.values)
	writeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: runtime.nativeID, HookEventName: "sessionStart", Source: "startup"})
	require.NoError(t, runtime.consumeObservations(context.Background()))
	require.Len(t, sink.values, 1)
	require.Equal(t, ports.PrimaryContextInitial, sink.values[0].Disposition)
	require.True(t, runtime.contextReady)
}

func writeHookEvent(t *testing.T, directory string, event sessionStartEvent) {
	t.Helper()
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	command := exec.Command("/bin/sh", "-c", observationCommand)
	command.Env = host.MergeEnvironment(os.Environ(), []string{"TCLAUDE_OBSERVATION_SPOOL=" + directory})
	command.Stdin = bytes.NewReader(raw)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}
