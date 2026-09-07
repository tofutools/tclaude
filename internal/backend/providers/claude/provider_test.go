//go:build linux || darwin

package claude

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

type observationSink struct {
	values []ports.PrimaryContextEvidence
}

func (s *observationSink) ObservePrimaryContext(_ context.Context, evidence ports.PrimaryContextEvidence) error {
	s.values = append(s.values, evidence)
	return nil
}

type failOnceObservationSink struct {
	calls    int
	accepted int
}

func (s *failOnceObservationSink) ObservePrimaryContext(_ context.Context, _ ports.PrimaryContextEvidence) error {
	s.calls++
	if s.calls == 1 {
		return os.ErrInvalid
	}
	s.accepted++
	return nil
}

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
	bootstrapPath := filepath.Join(root, "bootstrap")
	executable := filepath.Join(root, "claude-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CLAUDE_TEST_ARGV\"\nprintf '%s\\n' \"$TCLAUDE_BACKEND_SOCKET\" > \"$CLAUDE_TEST_BOOTSTRAP\"\ncat \"$TCLAUDE_BACKEND_CREDENTIAL_FILE\" >> \"$CLAUDE_TEST_BOOTSTRAP\"\nwhile IFS= read -r line; do\n  printf '%s\\n' \"$line\" >> \"$CLAUDE_TEST_INPUT\"\ndone\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	require.NoError(t, os.Setenv("CLAUDE_TEST_ARGV", argvPath))
	require.NoError(t, os.Setenv("CLAUDE_TEST_INPUT", inputPath))
	require.NoError(t, os.Setenv("CLAUDE_TEST_BOOTSTRAP", bootstrapPath))
	t.Cleanup(func() {
		_ = os.Unsetenv("CLAUDE_TEST_ARGV")
		_ = os.Unsetenv("CLAUDE_TEST_INPUT")
		_ = os.Unsetenv("CLAUDE_TEST_BOOTSTRAP")
	})

	agentSocket := filepath.Join(root, "backend.sock")
	provider, err := New(Config{Executable: executable, PrivateRoot: root, AgentSocket: agentSocket})
	require.NoError(t, err)
	request := ports.PreparationRequest{
		Intent:       ports.StartFresh,
		InitialInput: &ports.PreparedInitialInput{Body: "prepared claude brief", Correlation: "brief-claude", RequiredBeforeFirstWork: true},
		ActionCredential: &ports.ActionCredentialMaterial{
			ExecutionID: "execution_claude", Generation: 1, DeliveryID: "delivery-claude",
			Secret: []byte("claude-provider-secret"), ExpiresAt: time.Now().Add(time.Hour),
		},
		Spec: model.ResolvedExecutionSpec{
			ExecutionID: "execution_claude", Attempt: 2, Harness: Name, Model: "test-model", Effort: "high",
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
	require.NotNil(t, description.AccessDelivery)
	require.Equal(t, &ports.PreparedInitialInputDescription{Correlation: "brief-claude", Supported: true}, description.InitialInput)
	require.NotContains(t, string(description.Evidence.Payload), "claude-provider-secret")
	access := &model.ExecutionAccessBinding{ExecutionID: request.Spec.ExecutionID, Generation: 1,
		DeliveryID: "delivery-claude", State: model.ExecutionAccessSuspended, ExpiresAt: request.ActionCredential.ExpiresAt}

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
		Attempt: request.Spec.Attempt, Access: access,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, preparedRecovery.State,
		"the evidence persisted before release must recover a start completed before receipt persistence")
	require.NotNil(t, preparedRecovery.AccessProof)

	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(argvPath)
		return readErr == nil && strings.Contains(string(value), "--session-id") &&
			strings.Contains(string(value), "--permission-mode\nmanual") && strings.Contains(string(value), "prepared claude brief") && strings.Contains(string(value), "--effort\nhigh")
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		value, readErr := os.ReadFile(bootstrapPath)
		return readErr == nil && string(value) == agentSocket+"\nclaude-provider-secret"
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
		Attempt: request.Spec.Attempt, Access: access,
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
	controlled := recovered.Runtime.(*Runtime)
	controlled.observations = &observationSink{}
	_, exited, err := controlled.terminal.Stop(ctx, true)
	require.NoError(t, err)
	require.True(t, exited)
	observation, err = recovered.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadExited, observation.Workload)
	recorded, err := decodeEvidence(released.Evidence)
	require.NoError(t, err)
	require.NoFileExists(t, description.AccessDelivery.Resource)
	require.NoDirExists(t, recorded.ObservationSpool)
	observation, err = recovered.Runtime.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadExited, observation.Workload,
		"repeated exit observation must not reopen the removed ingress spool")
	afterCleanup, err := provider.Recover(context.Background(), ports.RecoveryRequest{
		ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: released.Evidence,
		Attempt: request.Spec.Attempt, Access: access,
	})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryExited, afterCleanup.State,
		"confirmed exit remains recoverable after resource cleanup")
	stopped, err := recovered.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.True(t, stopped.Exited)
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

func TestClaudeSessionStartIngressClassifiesPrimaryAndRejectsNestedEvents(t *testing.T) {
	spool, err := host.PrepareObservationSpool(filepath.Join(t.TempDir(), "observations"))
	require.NoError(t, err)
	sink := &observationSink{}
	initialID := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	runtime := &Runtime{
		executionID: "execution_observed", attempt: 4, nativeID: initialID,
		intent: ports.StartFresh, observations: sink, spool: spool,
	}

	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: initialID, HookEventName: "SessionStart", Source: "startup",
	})
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, runtime.contextReady)
	require.Len(t, sink.values, 1)
	require.Equal(t, ports.PrimaryContextInitial, sink.values[0].Disposition)
	require.Equal(t, model.AttemptGeneration(4), sink.values[0].Attempt)

	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: initialID, HookEventName: "SessionStart", Source: "startup", AgentID: "nested-agent",
	})
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, sink.values, 1, "subagent hook input must not become primary evidence")

	unexpectedID := "27277385-c996-4311-98b8-e3f677660207"
	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: unexpectedID, HookEventName: "SessionStart", Source: "resume",
	})
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, initialID, runtime.nativeID)
	require.Equal(t, ports.PrimaryContextUnresolved, sink.values[1].Disposition)
}

func TestClaudeClearRequiresPendingTransitionBeforeRotatingNativeBinding(t *testing.T) {
	spool, err := host.PrepareObservationSpool(filepath.Join(t.TempDir(), "observations"))
	require.NoError(t, err)
	sink := &observationSink{}
	initialID := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	nextID := "27277385-c996-4311-98b8-e3f677660207"
	runtime := &Runtime{
		executionID: "execution_reset", attempt: 8, nativeID: initialID, contextReady: true,
		intent: ports.StartFresh, observations: sink, spool: spool,
	}

	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: nextID, HookEventName: "SessionStart", Source: "clear",
	})
	confirmed, err := runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.False(t, confirmed)
	require.Equal(t, initialID, runtime.nativeID)
	require.Equal(t, ports.PrimaryContextUnresolved, sink.values[0].Disposition)

	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: nextID, HookEventName: "SessionStart", Source: "clear",
	})
	confirmed, err = runtime.consumeObservationEvents(context.Background(), &ports.ContextChange{
		Intent: ports.ContextReset, ExpectedConversation: "conversation_old", ExpectedAssociationRevision: 7,
		TransitionCorrelation: "transition-issued-by-app",
	})
	require.NoError(t, err)
	require.True(t, confirmed)
	require.Equal(t, nextID, runtime.nativeID)
	require.Equal(t, ports.PrimaryContextReset, sink.values[1].Disposition)
	require.Equal(t, "transition-issued-by-app", sink.values[1].TransitionCorrelation)
	require.Equal(t, model.ConversationID("conversation_old"), sink.values[1].ExpectedConversation)
	require.Equal(t, model.Revision(7), sink.values[1].ExpectedAssociationRevision)
	require.Equal(t, sink.values[0].ProviderOrder, sink.values[1].PriorProviderOrder)
}

func TestClaudeObservationRetriesAfterSinkFailure(t *testing.T) {
	spool, err := host.PrepareObservationSpool(filepath.Join(t.TempDir(), "observations"))
	require.NoError(t, err)
	sink := &failOnceObservationSink{}
	id := "43e874eb-4827-4b22-b1b8-376a5e5e553f"
	runtime := &Runtime{executionID: "execution_retry", attempt: 1, nativeID: id,
		intent: ports.StartFresh, observations: sink, spool: spool}
	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: id, HookEventName: "SessionStart", Source: "startup",
	})

	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.Error(t, err)
	_, err = runtime.consumeObservationEvents(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, runtime.contextReady)
	require.Equal(t, 1, sink.accepted)
}

func TestClaudeQueuedClearCannotConfirmNewRequest(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tclaude-claude-transition-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	terminalHost := host.TerminalHost{PrivateRoot: root}
	preparedTerminal, err := terminalHost.Prepare("transition-test")
	require.NoError(t, err)
	terminal, err := preparedTerminal.Release(host.ProcessSpec{
		Executable: "/bin/sh", Args: []string{"-c", "while IFS= read -r line; do :; done"}, Directory: root,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = terminal.Stop(ctx, true)
	})
	spool, err := host.PrepareObservationSpool(filepath.Join(root, "observations"))
	require.NoError(t, err)
	sink := &observationSink{}
	runtime := &Runtime{executionID: "execution_transition", attempt: 1,
		nativeID: "43e874eb-4827-4b22-b1b8-376a5e5e553f", contextReady: true,
		intent: ports.StartFresh, observations: sink, spool: spool, terminal: terminal}
	writeClaudeHookEvent(t, spool.Directory(), sessionStartEvent{
		SessionID: "27277385-c996-4311-98b8-e3f677660207", HookEventName: "SessionStart", Source: "clear",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, _ := runtime.ChangeContext(ctx, ports.ContextChange{
		Intent: ports.ContextReset, TransitionCorrelation: "new-request",
	})
	require.NotEqual(t, ports.EffectAccepted, result.Disposition)
	require.Len(t, sink.values, 1)
	require.Equal(t, ports.PrimaryContextUnresolved, sink.values[0].Disposition)
}

func writeClaudeHookEvent(t *testing.T, directory string, event sessionStartEvent) {
	t.Helper()
	payload, err := json.Marshal(event)
	require.NoError(t, err)
	command := exec.Command("/bin/sh", "-c", claudeObservationCommand)
	command.Env = host.MergeEnvironment(os.Environ(), []string{"TCLAUDE_OBSERVATION_SPOOL=" + directory})
	command.Stdin = bytes.NewReader(payload)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}
