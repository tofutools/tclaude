package opencode

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func TestProviderHostSandboxOpenCodeLaunchControlAttachmentAndRecovery(t *testing.T) {
	bootstrap := os.Getenv("TCLAUDE_SANDBOX_BOOTSTRAP")
	if bootstrap == "" {
		t.Skip("native CI provides the shipped sandbox bootstrap")
	}
	wrapperName := "bwrap"
	if runtime.GOOS == "darwin" {
		wrapperName = "sandbox-exec"
	}
	wrapper, err := exec.LookPath(wrapperName)
	require.NoError(t, err)
	root, err := os.MkdirTemp("/tmp", "oc-provider-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	private, workspace := filepath.Join(root, "private"), filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(workspace, 0700))
	secret := filepath.Join(private, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("private fixture"), 0600))
	nativeData := filepath.Join(private, "native-data")
	require.NoError(t, os.Mkdir(nativeData, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeData, "auth.json"), []byte("fixture login"), 0600))
	nativeConfig := filepath.Join(private, "native-config", "opencode")
	require.NoError(t, os.MkdirAll(nativeConfig, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(nativeConfig, "opencode.json"), []byte("fixture configuration"), 0600))
	binary, err := os.Executable()
	require.NoError(t, err)
	native := filepath.Join(private, "native-fixture")
	script := `#!/bin/sh
if [ "$1" = export ]; then
  if [ -f "$XDG_DATA_HOME/export.json" ]; then cat "$XDG_DATA_HOME/export.json"; else cat "$OPENCODE_EXPORT_FIXTURE"; fi
  exit
fi
if [ "$1" = import ]; then
  if cat "$PRIVATE_FIXTURE" >/dev/null 2>&1; then exit 81; fi
  test -d "$XDG_DATA_HOME/opencode" || exit 82
  cp "$2" "$XDG_DATA_HOME/export.json" || exit 83
  printf imported >> "$IMPORT_MARKER"
  exit
fi
if [ "$1" = serve ]; then
  if cat "$PRIVATE_FIXTURE" >/dev/null 2>&1; then exit 81; fi
  test "$(cat "$XDG_DATA_HOME/opencode/auth.json")" = 'fixture login' || exit 84
  test "$(cat "$XDG_CONFIG_HOME/opencode/opencode.json")" = 'fixture configuration' || exit 85
  test -f "$XDG_CONFIG_HOME/opencode/.gitignore" || exit 86
  if touch "$XDG_CONFIG_HOME/opencode/forbidden-write" 2>/dev/null; then exit 87; fi
  printf '%s' "${TCLAUDE_UNRELATED_DAEMON_VALUE-absent}" > "$ENVIRONMENT_OUTPUT"
fi
exec "$OPENCODE_TEST_BINARY" -test.run=^TestOpenCodeServerHelper$ -- "$@"
`
	require.NoError(t, os.WriteFile(native, []byte(script), 0700))
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	planner, err := host.NewSandboxLaunchPreparer(host.SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: bootstrap, Artifacts: private})
	require.NoError(t, err)
	policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{
		{HostPath: workspace, Access: model.SandboxFilesystemWrite},
		{HostPath: binary, Access: model.SandboxFilesystemRead},
	}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}}
	hash, err := sandboxpolicy.ContentHash(policy)
	require.NoError(t, err)
	materialized, err := sandboxpolicy.MaterializeScopes(context.Background(), []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: "control", RevisionID: "revision", ContentHash: hash}}}, controlPolicyReader{policy}, inspector)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	t.Setenv("TCLAUDE_UNRELATED_DAEMON_VALUE", "must not inherit")
	prompt := filepath.Join(workspace, "prompt.json")
	exportPath := filepath.Join(workspace, "source-export.json")
	writeOpenCodeExport(t, exportPath, "ses_test", workspace, "source answer")
	importMarker, forkPoint := filepath.Join(workspace, "import-marker"), filepath.Join(workspace, "fork-point")
	provider, err := New(Config{Executable: native, PrivateRoot: filepath.Join(private, "provider"), HostSandbox: planner, NativeDataDirectory: nativeData, NativeConfigDirectory: nativeConfig,
		Environment: []string{"OPENCODE_TEST_BINARY=" + binary, "OPENCODE_TEST_PROMPT=" + prompt, "PRIVATE_FIXTURE=" + secret, "ENVIRONMENT_OUTPUT=" + filepath.Join(workspace, "environment"), "OPENCODE_EXPORT_FIXTURE=" + exportPath, "IMPORT_MARKER=" + importMarker, "OPENCODE_TEST_FORK_POINT=" + forkPoint}})
	require.NoError(t, err)
	observations := &observationSink{}
	request := ports.PreparationRequest{Observations: observations, Intent: ports.StartFresh, HostSandboxPolicy: &materialized,
		Spec:         model.ResolvedExecutionSpec{ExecutionID: "execution_confined", Attempt: 1, Harness: Name, WorkingDirectory: workspace, Model: "provider/model", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined, HostSandbox: &selected},
		InitialInput: &ports.PreparedInitialInput{Body: "first confined work", Correlation: "brief", RequiredBeforeFirstWork: true}}
	attempt, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	description := attempt.Describe()
	require.Equal(t, selected.PolicyHash, description.HostSandboxPolicyHash)
	output, err := os.Create(filepath.Join(workspace, "server-output"))
	require.NoError(t, err)
	attempt.(*prepared).command.Stdout, attempt.(*prepared).command.Stderr = output, output
	t.Cleanup(func() {
		_ = output.Close()
		if t.Failed() {
			data, _ := os.ReadFile(output.Name())
			t.Logf("native provider output: %s", data)
		}
	})
	permit := &testPermit{execution: request.Spec.ExecutionID, operation: "operation_confined"}
	released, err := attempt.Release(context.Background(), permit)
	if released.Runtime != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
		})
	}
	require.NoError(t, err)
	require.Equal(t, ports.ReleaseStarted, released.State)
	environment, err := os.ReadFile(filepath.Join(workspace, "environment"))
	require.NoError(t, err)
	require.Equal(t, "absent", string(environment))
	input, err := os.ReadFile(prompt)
	require.NoError(t, err)
	require.Contains(t, string(input), "first confined work")
	// Prepared evidence has no process or socket identity yet. Recovery must
	// find the one marked root and adopt only its retained bootstrap endpoint.
	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{ExecutionID: request.Spec.ExecutionID, Attempt: request.Spec.Attempt, Spec: request.Spec, Evidence: description.Evidence, Observations: observations})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	controlled := recovered.Runtime.(*Runtime)
	endpoint, closeRelay, err := controlled.sandboxAttachmentEndpoint(context.Background())
	require.NoError(t, err)
	defer closeRelay()
	query, err := http.NewRequest(http.MethodGet, endpoint+"/global/health", nil)
	require.NoError(t, err)
	query.SetBasicAuth(serverUsername, controlled.password)
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(query)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	closeRelay()
	attachment, err := controlled.Attach(context.Background(), ports.AttachmentRequest{Kind: ports.AttachmentTerminal})
	require.NoError(t, err)
	require.Equal(t, ports.EffectAccepted, attachment.Disposition)
	require.NoError(t, attachment.Attachment.Close())
	observation, err := controlled.Observe(context.Background())
	require.NoError(t, err)
	require.Equal(t, ports.WorkloadRunning, observation.Workload)
	require.Equal(t, ports.ContextReady, observation.Context)
	repeated, err := attempt.Release(context.Background(), &testPermit{execution: request.Spec.ExecutionID, operation: "operation_confined"})
	require.Error(t, err)
	require.Nil(t, repeated.Runtime)
	stopRuntime := func(value ports.Runtime) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := value.Stop(ctx, ports.StopRequest{Force: true})
		require.NoError(t, err)
	}
	stopRuntime(controlled)
	continuedRequest := request
	continuedRequest.Intent, continuedRequest.InitialInput = ports.StartContinue, nil
	continuedRequest.Spec.ExecutionID = "execution_continued"
	continuedRequest.Continuation = &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: "ses_test"}
	continuedRequest.PriorEvidence = recovered.Evidence
	continued, err := provider.Prepare(context.Background(), continuedRequest)
	require.NoError(t, err)
	continued.(*prepared).command.Stdout, continued.(*prepared).command.Stderr = output, output
	continuation, err := continued.Release(context.Background(), &testPermit{execution: continuedRequest.Spec.ExecutionID, operation: "operation_continue"})
	if continuation.Runtime != nil {
		t.Cleanup(func() { stopRuntime(continuation.Runtime) })
	}
	require.NoError(t, err)
	require.Equal(t, ports.ReleaseStarted, continuation.State)
	priorState, err := decodeEvidence(recovered.Evidence)
	require.NoError(t, err)
	continuedState, err := decodeEvidence(continuation.Evidence)
	require.NoError(t, err)
	require.Equal(t, priorState.StateRoot, continuedState.StateRoot)
	require.Equal(t, priorState.NativeID, continuedState.NativeID)
	require.NotEqual(t, priorState.HostSandbox, continuedState.HostSandbox, "new execution retains a new one-shot launch")
	stopRuntime(continuation.Runtime)
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	source := discovered.Histories[0]
	selection := &ports.HistorySourceSelection{ConversationID: "conversation_source", Provider: Name, Native: source.Native,
		SourceToken: source.SourceToken, SourceRevision: source.Coverage.SourceRevision, SourceFingerprint: source.SourceFingerprint,
		Point: &source.Points[1], Evidence: source.Evidence}
	selection.UseClaim = &model.HistoryUseClaim{ID: "history_use", ConversationID: selection.ConversationID,
		OperationID: "operation_fork", SourceRevision: selection.SourceRevision, SourceFingerprint: selection.SourceFingerprint, State: model.HistoryUseHeld}
	forkRequest := request
	forkRequest.Intent, forkRequest.InitialInput, forkRequest.History = ports.StartFork, nil, selection
	forkRequest.Spec.ExecutionID = "execution_forked"
	forked, err := provider.Prepare(context.Background(), forkRequest)
	require.NoError(t, err)
	require.NoFileExists(t, importMarker, "selected fork preparation must not run native import")
	forked.(*prepared).command.Stdout, forked.(*prepared).command.Stderr = output, output
	fork, err := forked.Release(context.Background(), &testPermit{execution: forkRequest.Spec.ExecutionID, operation: "operation_fork"})
	if fork.Runtime != nil {
		t.Cleanup(func() { stopRuntime(fork.Runtime) })
	}
	require.NoError(t, err)
	require.Equal(t, ports.ReleaseStarted, fork.State)
	forkState, err := decodeEvidence(fork.Evidence)
	require.NoError(t, err)
	require.NotEqual(t, continuedState.StateRoot, forkState.StateRoot)
	require.Equal(t, "ses_fork", forkState.NativeID)
	require.Equal(t, "ses_test", forkState.ForkSourceID)
	point, err := os.ReadFile(forkPoint)
	require.NoError(t, err)
	require.Equal(t, "msg_two", string(point))
	marker, err := os.ReadFile(importMarker)
	require.NoError(t, err)
	require.Equal(t, "imported", string(marker))
	require.Equal(t, "source answer", mustExportSecondText(t, exportPath))
}
