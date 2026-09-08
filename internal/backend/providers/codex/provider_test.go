//go:build linux || darwin

package codex

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
	root, err := os.MkdirTemp("/tmp", "tcl-codex-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	argvPath := filepath.Join(root, "argv")
	inputPath := filepath.Join(root, "input")
	bootstrapPath := filepath.Join(root, "bootstrap")
	nativeHome := filepath.Join(root, "native-home")
	require.NoError(t, os.MkdirAll(nativeHome, 0o700))
	authPath := filepath.Join(nativeHome, "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"fixture-only"}}`), 0o600))
	executable := filepath.Join(root, "codex-fake")
	script := "#!/bin/sh\nprintf '%s' \"$APP_ENV_PROBE\" > \"$APP_ENV_OUTPUT\"\nprintf '%s\\n' \"$@\" > \"$CODEX_TEST_ARGV\"\nprintf '%s\\n' \"$CODEX_HOME\" \"$TCLAUDE_BACKEND_SOCKET\" > \"$CODEX_TEST_BOOTSTRAP\"\ncat \"$TCLAUDE_BACKEND_CREDENTIAL_FILE\" >> \"$CODEX_TEST_BOOTSTRAP\"\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> \"$CODEX_TEST_INPUT\"; done\n"
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	t.Setenv("CODEX_TEST_ARGV", argvPath)
	t.Setenv("CODEX_TEST_INPUT", inputPath)
	t.Setenv("CODEX_TEST_BOOTSTRAP", bootstrapPath)
	provider, err := New(Config{Executable: executable, PrivateRoot: root, NativeHome: nativeHome, AgentSocket: filepath.Join(root, "agent.sock")})
	require.NoError(t, err)
	expires := time.Now().Add(time.Hour)
	request := ports.PreparationRequest{Intent: ports.StartFresh, InitialInput: &ports.PreparedInitialInput{Body: "prepared codex brief", Correlation: "brief-codex", RequiredBeforeFirstWork: true}, Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_codex", Attempt: 3, Harness: Name, Model: "test-model", Effort: "high", WorkingDirectory: root, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxUnconfined}, ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "execution_codex", Generation: 1, DeliveryID: "delivery", Secret: []byte("codex-secret"), ExpiresAt: expires}}
	request.Spec.Environment = model.Environment{"APP_ENV_PROBE": "literal $HOME\nwith=equals", "APP_ENV_OUTPUT": filepath.Join(root, "environment")}
	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	description := prepared.Describe()
	require.Equal(t, ports.TopologyTerminalAuthoritative, description.Topology)
	require.Equal(t, []model.SandboxMode{model.SandboxReadOnly, model.SandboxWorkspaceWrite, model.SandboxUnconfined}, description.Requirements.Policy.SupportedSandbox)
	require.NotContains(t, string(description.Evidence.Payload), "codex-secret")
	require.Equal(t, &ports.PreparedInitialInputDescription{Correlation: "brief-codex", Supported: true}, description.InitialInput)
	permit := &testPermit{execution: request.Spec.ExecutionID}
	released, err := prepared.Release(context.Background(), permit)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(root, "environment"))
		return err == nil && string(raw) == "literal $HOME\nwith=equals"
	}, 5*time.Second, 10*time.Millisecond)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(argvPath)
		return readErr == nil && strings.Contains(string(raw), "never") && strings.Contains(string(raw), "danger-full-access") && strings.Contains(string(raw), "prepared codex brief") && strings.Contains(string(raw), `model_reasoning_effort="high"`)
	}, time.Second, 10*time.Millisecond)
	filePermit := &testPermit{execution: request.Spec.ExecutionID}
	staged, err := released.Runtime.(ports.TerminalFileStager).StageTerminalFile(context.Background(), ports.StageTerminalFileRequest{ExecutionID: request.Spec.ExecutionID, OperationID: filePermit.OperationID(), Filename: "drawing.png", Content: []byte("native user file"), Permit: filePermit})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, staged.Disposition)
	userFile, err := os.ReadFile(staged.NativePath)
	require.NoError(t, err)
	require.Equal(t, "native user file", string(userFile))
	require.Equal(t, filepath.Join(root, "terminals", "uploads"), filepath.Dir(staged.NativePath))

	assertNativeTurnActivity(t, released.Runtime.(*Runtime), request, released.Evidence)

	interaction, err := released.Runtime.Interact(context.Background(), ports.Interaction{Text: "literal $(touch nope); `false`"})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, interaction.Disposition)
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(inputPath)
		return readErr == nil && strings.Contains(string(raw), "literal $(touch nope); `false`")
	}, time.Second, 10*time.Millisecond)
	require.NoFileExists(t, filepath.Join(root, "nope"))
	auth, err := os.ReadFile(authPath)
	require.NoError(t, err)
	require.Contains(t, string(auth), "fixture-only", "provider-owned native login resource remains native-managed")
	access := &model.ExecutionAccessBinding{ExecutionID: request.Spec.ExecutionID, Generation: 1, DeliveryID: "delivery", State: model.ExecutionAccessSuspended, ExpiresAt: expires}
	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Evidence: released.Evidence, Attempt: request.Spec.Attempt, Access: access})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	require.NotNil(t, recovered.AccessProof)
}

func TestProviderMapsRequestedNativeConfinement(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tcl-codex-policy-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root})
	require.NoError(t, err)
	prepared, err := provider.Prepare(context.Background(), ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{ExecutionID: "execution", Harness: Name, WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	require.Equal(t, model.SandboxWorkspaceWrite, prepared.Describe().EffectivePolicy.Sandbox)
	require.NoError(t, prepared.Abort(context.Background()))
}

func TestSessionStartObservationRequiresExpectedPrimarySession(t *testing.T) {
	spool, err := host.PrepareObservationSpool(filepath.Join(t.TempDir(), "observations"))
	require.NoError(t, err)
	sink := &observationSink{}
	runtime := &Runtime{executionID: "execution", attempt: 2, intent: ports.StartFresh, observations: sink, spool: spool}
	writeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: "not-a-uuid", HookEventName: "SessionStart", Source: "startup"})
	require.NoError(t, runtime.consumeObservations(context.Background()))
	require.Empty(t, sink.values)
	writeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: "00000000-0000-4000-8000-000000000001", HookEventName: "SessionStart", Source: "startup"})
	require.NoError(t, runtime.consumeObservations(context.Background()))
	require.Len(t, sink.values, 1)
	require.Equal(t, ports.PrimaryContextInitial, sink.values[0].Disposition)
	require.True(t, runtime.contextReady)
	require.Equal(t, "00000000-0000-4000-8000-000000000001", runtime.nativeID)
	writeHookEvent(t, spool.Directory(), sessionStartEvent{SessionID: "00000000-0000-4000-8000-000000000002", HookEventName: "SessionStart", Source: "clear"})
	require.NoError(t, runtime.consumeObservations(context.Background()))
	require.Len(t, sink.values, 2)
	require.Equal(t, ports.PrimaryContextUnresolved, sink.values[1].Disposition)
	require.Equal(t, "00000000-0000-4000-8000-000000000001", sink.values[1].PriorBinding.Reference)
	require.Equal(t, "00000000-0000-4000-8000-000000000002", runtime.nativeID)
	require.False(t, runtime.contextReady)
}

func TestAcceptedApplicationContinuationEvidence(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tcl-codex-resume-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	provider, err := New(Config{Executable: os.Args[0], PrivateRoot: root})
	require.NoError(t, err)
	nativeID := "00000000-0000-4000-8000-000000000001"
	prior, err := encodeEvidence(evidence{ExecutionID: "prior", NativeID: nativeID, StateRoot: provider.nativeHome, ObservationSpool: filepath.Join(root, "prior-spool")})
	require.NoError(t, err)
	prepared, err := provider.Prepare(context.Background(), ports.PreparationRequest{Intent: ports.StartContinue, Continuation: &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: nativeID}, PriorEvidence: prior, Spec: model.ResolvedExecutionSpec{ExecutionID: "resumed", Harness: Name, WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxReadOnly}})
	require.NoError(t, err)
	require.NoError(t, prepared.Abort(context.Background()))
}

func TestConcurrentExecutionsKeepPolicyAndSpoolsExecutionScoped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "tcl-codex-concurrent-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	nativeHome := filepath.Join(root, "native-home")
	require.NoError(t, os.MkdirAll(nativeHome, 0o700))
	authPath := filepath.Join(nativeHome, "auth.json")
	require.NoError(t, os.WriteFile(authPath, []byte("fixture-auth"), 0o600))
	executable := filepath.Join(root, "codex-fake")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PWD/argv\"\nwhile IFS= read -r line; do :; done\n"), 0o700))
	provider, err := New(Config{Executable: executable, PrivateRoot: root, NativeHome: nativeHome})
	require.NoError(t, err)
	cwdOne, cwdTwo := filepath.Join(root, "one"), filepath.Join(root, "two")
	require.NoError(t, os.Mkdir(cwdOne, 0o700))
	require.NoError(t, os.Mkdir(cwdTwo, 0o700))
	launch := func(id model.ExecutionID, cwd string, approval model.ApprovalMode, sandbox model.SandboxMode) ports.ReleaseResult {
		prepared, prepareErr := provider.Prepare(context.Background(), ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{ExecutionID: id, Attempt: 1, Harness: Name, WorkingDirectory: cwd, Approval: approval, Sandbox: sandbox}})
		require.NoError(t, prepareErr)
		released, releaseErr := prepared.Release(context.Background(), &testPermit{execution: id})
		require.NoError(t, releaseErr)
		return released
	}
	one := launch("execution_one", cwdOne, model.ApprovalAutomatic, model.SandboxUnconfined)
	two := launch("execution_two", cwdTwo, model.ApprovalSupervised, model.SandboxReadOnly)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = two.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})
	require.Eventually(t, func() bool {
		first, e1 := os.ReadFile(filepath.Join(cwdOne, "argv"))
		second, e2 := os.ReadFile(filepath.Join(cwdTwo, "argv"))
		return e1 == nil && e2 == nil && strings.Contains(string(first), "never") && strings.Contains(string(first), "danger-full-access") && strings.Contains(string(second), "on-request") && strings.Contains(string(second), "read-only")
	}, time.Second, 10*time.Millisecond)
	oneEvidence, err := decodeEvidence(one.Evidence)
	require.NoError(t, err)
	twoEvidence, err := decodeEvidence(two.Evidence)
	require.NoError(t, err)
	require.NotEqual(t, oneEvidence.ObservationSpool, twoEvidence.ObservationSpool)
	require.Equal(t, nativeHome, oneEvidence.StateRoot)
	require.Equal(t, nativeHome, twoEvidence.StateRoot)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = one.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
	require.FileExists(t, authPath)
	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{ExecutionID: "execution_two", Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_two", Harness: Name, WorkingDirectory: cwdTwo, Approval: model.ApprovalSupervised, Sandbox: model.SandboxReadOnly}, Evidence: two.Evidence, Attempt: 1})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	require.FileExists(t, authPath)
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
