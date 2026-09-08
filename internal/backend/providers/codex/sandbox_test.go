//go:build linux || darwin

package codex

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type sandboxPolicyReader struct{ policy model.SandboxPolicy }

func (r sandboxPolicyReader) ReadSandboxRevision(context.Context, model.SandboxProfileRef) (model.SandboxPolicy, error) {
	return r.policy, nil
}

func TestProviderHostSandboxPreparesExactCommandAndRefusesChangedCredentialBeforeRelease(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("/tmp", "sb-codex-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	artifacts := filepath.Join(root, "artifacts")
	require.NoError(t, os.Mkdir(artifacts, 0700))
	inspector, err := host.NewSandboxPathInspector([]string{root})
	require.NoError(t, err)
	// Neither executable is run by this preparation test. Actual OS enforcement
	// and Unix socket access are exercised by the required native host journeys.
	planner, err := host.NewSandboxLaunchPreparer(host.SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: artifacts})
	require.NoError(t, err)
	socket := filepath.Join(root, "api.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	provider, err := New(Config{Executable: "/bin/sh", PrivateRoot: filepath.Join(root, "provider"), AgentSocket: socket, HostSandbox: planner})
	require.NoError(t, err)
	require.True(t, provider.Capabilities().HostSandbox)
	workspace := t.TempDir()
	policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{{HostPath: workspace, Access: model.SandboxFilesystemWrite}}, Environment: model.Environment{"LITERAL": "$HOME exact"}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}}
	hash, err := sandboxpolicy.ContentHash(policy)
	require.NoError(t, err)
	ref := model.SandboxProfileRef{ProfileID: "policy", RevisionID: "revision", ContentHash: hash}
	materialized, err := sandboxpolicy.MaterializeScopes(ctx, []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: ref}}, sandboxPolicyReader{policy}, inspector)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	t.Setenv("UNSELECTED_DAEMON_VALUE", "must not be copied")
	request := ports.PreparationRequest{NativeGuidance: sandboxGuidance{}, CallbackIngress: sandboxIngress{socket}, HostSandboxPolicy: &materialized, Spec: model.ResolvedExecutionSpec{HostSandbox: &selected, ExecutionID: "execution", Attempt: 1, Harness: Name, Model: "fixture", WorkingDirectory: workspace, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}, Intent: ports.StartFresh, InitialInput: &ports.PreparedInitialInput{Body: "Exact first work", Correlation: "brief", RequiredBeforeFirstWork: true}, ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "execution", Generation: 1, DeliveryID: "delivery", Secret: []byte("disposable action credential"), ExpiresAt: time.Now().Add(time.Hour)}}
	attempt, err := provider.Prepare(ctx, request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = attempt.Abort(context.Background()) })
	description := attempt.Describe()
	require.Equal(t, selected.PolicyHash, description.HostSandboxPolicyHash)
	recorded, err := decodeEvidence(description.Evidence)
	require.NoError(t, err)
	require.NotNil(t, recorded.HostSandbox)
	require.Equal(t, selected.PolicyHash, recorded.HostSandboxPolicyHash)
	require.NoError(t, host.VerifySandboxChild(ctx, *recorded.HostSandbox))
	require.NoFileExists(t, recorded.HostSandbox.Path+".started")
	data, err := os.ReadFile(recorded.HostSandbox.Path)
	require.NoError(t, err)
	var command struct {
		Arguments, Environment []string
		ProviderResources      []host.SandboxMountPin
	}
	require.NoError(t, json.Unmarshal(data, &command))
	require.Contains(t, command.Arguments, "Exact first work")
	require.Contains(t, command.Environment, "LITERAL=$HOME exact")
	require.Contains(t, command.Environment, "CODEX_HOME="+provider.nativeHome)
	require.NotContains(t, string(data), "must not be copied")
	require.NotContains(t, string(data), "disposable action credential")
	require.Len(t, command.ProviderResources, 8)
	// Preparing the same shared home must not replace the pinned hooks file.
	require.NoError(t, provider.prepareStateRoot(provider.nativeHome, workspace))
	require.NoError(t, host.VerifySandboxChild(ctx, *recorded.HostSandbox))
	require.NoError(t, os.Rename(recorded.Access.Resource, recorded.Access.Resource+".old"))
	require.NoError(t, os.WriteFile(recorded.Access.Resource, []byte("replacement"), 0600))
	permit := &testPermit{execution: request.Spec.ExecutionID}
	_, err = attempt.Release(ctx, permit)
	require.ErrorContains(t, err, "identity changed")
	require.False(t, permit.consumed.Load())
	require.NoFileExists(t, recorded.HostSandbox.Path+".started")
	require.NoError(t, attempt.Abort(ctx))
	require.NoDirExists(t, filepath.Dir(recorded.HostSandbox.Path))
	require.DirExists(t, provider.nativeHome)
	request.Intent = ports.StartFork
	request.History = sandboxForkHistory(t, provider, workspace)
	forkAttempt, err := provider.Prepare(ctx, request)
	require.NoError(t, err)
	forkEvidence, err := decodeEvidence(forkAttempt.Describe().Evidence)
	require.NoError(t, err)
	require.NotEmpty(t, forkEvidence.ForkReceipt)
	require.NoFileExists(t, forkEvidence.ForkReceipt+".started", "preparation must not run the fork helper")
	require.NoError(t, host.VerifySandboxChild(ctx, *forkEvidence.HostSandbox))
	require.NoError(t, forkAttempt.Abort(ctx))
	require.NoDirExists(t, filepath.Dir(forkEvidence.ForkReceipt))
}

// A synthetic native executable exercises the production provider, terminal,
// shipped bootstrap and kernel wrapper. It performs no authenticated native
// harness work and uses only disposable credentials and directories.
func TestProviderHostSandboxNativeLaunchAndRecovery(t *testing.T) {
	for _, fork := range []bool{false, true} {
		name := "fresh"
		if fork {
			name = "fork"
		}
		t.Run(name, func(t *testing.T) { testProviderHostSandboxNativeLaunchAndRecovery(t, fork) })
	}
}
func testProviderHostSandboxNativeLaunchAndRecovery(t *testing.T, fork bool) {
	bootstrap := os.Getenv("TCLAUDE_SANDBOX_BOOTSTRAP")
	if bootstrap == "" {
		t.Skip("required native CI provides the shipped bootstrap")
	}
	wrapperName := "bwrap"
	if runtime.GOOS == "darwin" {
		wrapperName = "sandbox-exec"
	}
	wrapper, err := exec.LookPath(wrapperName)
	require.NoError(t, err)
	root, err := os.MkdirTemp("/tmp", "cx-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	private, workspace := filepath.Join(root, "private"), filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(workspace, 0700))
	artifacts := filepath.Join(private, "artifacts")
	require.NoError(t, os.Mkdir(artifacts, 0700))
	sibling := filepath.Join(private, "secret")
	require.NoError(t, os.WriteFile(sibling, []byte("private sibling"), 0600))
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(workspace, "client-error"))
			t.Logf("confined endpoint client: %s", data)
		}
	})
	executable := filepath.Join(workspace, "native-fixture")
	script := `#!/bin/sh
set -eu
test -r "$TCLAUDE_NATIVE_CALLBACK_SCRIPT"
test "$(cat "$TCLAUDE_BACKEND_CREDENTIAL_FILE")" = 'fixture credential'
if cat "$PRIVATE_SIBLING" >/dev/null 2>&1; then exit 24; fi
if test "$1" = app-server; then
 printf helper >> "$WORKSPACE/fork-calls"
 IFS= read -r initialize
 printf '%s\n' '{"id":1,"result":{}}'
 IFS= read -r initialized
 IFS= read -r fork
 printf '%s\n' "$fork" > "$WORKSPACE/fork-request"
 printf '%s\n' '{"id":2,"result":{"thread":{"id":"forked-thread"}}}'
 exit 0
fi
printf '%s\n' "$@" > "$WORKSPACE/args"
curl --fail --silent --show-error --max-time 5 --unix-socket "$CONTROL_SOCKET" http://fixture/ > "$WORKSPACE/first-response" 2> "$WORKSPACE/client-error"
printf ready > "$WORKSPACE/ready"
while test ! -f "$WORKSPACE/reconnect"; do sleep 0.05; done
curl --fail --silent --show-error --max-time 5 --unix-socket "$CONTROL_SOCKET" http://fixture/ > "$WORKSPACE/second-response" 2> "$WORKSPACE/client-error"
if cat "$PRIVATE_SIBLING" >/dev/null 2>&1; then exit 25; fi
printf reconnected > "$WORKSPACE/reconnected"
while IFS= read -r line; do printf '%s\n' "$line" >> "$WORKSPACE/input"; done
`
	require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
	control := filepath.Join(private, "api")
	require.NoError(t, os.Mkdir(control, 0700))
	socket := filepath.Join(control, "socket")
	serve := func(response string) *http.Server {
		listener, listenErr := net.Listen("unix", socket)
		require.NoError(t, listenErr)
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, response) })}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
		return server
	}
	firstServer := serve("before restart")
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	planner, err := host.NewSandboxLaunchPreparer(host.SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: bootstrap, Artifacts: artifacts})
	require.NoError(t, err)
	provider, err := New(Config{Executable: executable, PrivateRoot: filepath.Join(private, "provider"), AgentSocket: socket, AgentSocketDirectory: control, HostSandbox: planner})
	require.NoError(t, err)
	policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{{HostPath: workspace, Access: model.SandboxFilesystemWrite}}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}}
	hash, err := sandboxpolicy.ContentHash(policy)
	require.NoError(t, err)
	ref := model.SandboxProfileRef{ProfileID: "policy", RevisionID: "revision", ContentHash: hash}
	materialized, err := sandboxpolicy.MaterializeScopes(context.Background(), []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: ref}}, sandboxPolicyReader{policy}, inspector)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	request := ports.PreparationRequest{NativeGuidance: sandboxGuidance{}, CallbackIngress: sandboxIngress{socket}, HostSandboxPolicy: &materialized, Spec: model.ResolvedExecutionSpec{HostSandbox: &selected, ExecutionID: "native_execution", Attempt: 1, Harness: Name, Model: "fixture", WorkingDirectory: workspace, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, Environment: model.Environment{"PRIVATE_SIBLING": sibling, "WORKSPACE": workspace, "CONTROL_SOCKET": socket}}, Intent: ports.StartFresh, InitialInput: &ports.PreparedInitialInput{Body: "first work literal", Correlation: "brief", RequiredBeforeFirstWork: true}, ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "native_execution", Generation: 1, DeliveryID: "delivery", Secret: []byte("fixture credential"), ExpiresAt: time.Now().Add(time.Hour)}}
	if fork {
		request.Intent = ports.StartFork
		request.History = sandboxForkHistory(t, provider, workspace)
	}

	prepared, err := provider.Prepare(context.Background(), request)
	require.NoError(t, err)
	released, err := prepared.Release(context.Background(), &testPermit{execution: request.Spec.ExecutionID})
	require.NoError(t, err)
	require.NotNil(t, released.Runtime)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	})
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(workspace, "ready")); return err == nil }, 10*time.Second, 20*time.Millisecond)
	firstResponse, err := os.ReadFile(filepath.Join(workspace, "first-response"))
	require.NoError(t, err)
	require.Equal(t, "before restart", string(firstResponse))
	require.NoError(t, firstServer.Close())
	serve("after restart")
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "reconnect"), nil, 0600))
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(workspace, "reconnected")); return err == nil }, 10*time.Second, 20*time.Millisecond)
	secondResponse, err := os.ReadFile(filepath.Join(workspace, "second-response"))
	require.NoError(t, err)
	require.Equal(t, "after restart", string(secondResponse))
	args, err := os.ReadFile(filepath.Join(workspace, "args"))
	require.NoError(t, err)
	require.Contains(t, string(args), "first work literal")
	if fork {
		require.Contains(t, string(args), "resume\nforked-thread\n")
		calls, err := os.ReadFile(filepath.Join(workspace, "fork-calls"))
		require.NoError(t, err)
		require.Equal(t, "helper", string(calls))
		forkRequest, err := os.ReadFile(filepath.Join(workspace, "fork-request"))
		require.NoError(t, err)
		require.Contains(t, string(forkRequest), `"lastTurnId":"turn-7"`)
	}
	recorded, err := decodeEvidence(released.Evidence)
	require.NoError(t, err)
	require.NotNil(t, recorded.HostSandbox)
	require.Equal(t, selected.PolicyHash, recorded.HostSandboxPolicyHash)
	require.FileExists(t, recorded.HostSandbox.Path+".started")
	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{NativeGuidance: request.NativeGuidance, CallbackIngress: request.CallbackIngress, ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Attempt: 1, Evidence: released.Evidence})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	evidence, err := recovered.Runtime.(*Runtime).providerEvidence()
	require.NoError(t, err)
	retained, err := decodeEvidence(evidence)
	require.NoError(t, err)
	require.Equal(t, recorded.HostSandbox, retained.HostSandbox)
	if fork {
		require.Equal(t, "forked-thread", retained.NativeID)
		calls, err := os.ReadFile(filepath.Join(workspace, "fork-calls"))
		require.NoError(t, err)
		require.Equal(t, "helper", string(calls), "recovery must not repeat the native fork")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = recovered.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
}

type sandboxGuidance struct{}

func (sandboxGuidance) EvaluateNativeGuidance(context.Context, ports.NormalizedNativeEvent) (ports.NativeGuidanceAdmission, error) {
	return ports.NativeGuidanceAdmission{}, nil
}
func (sandboxGuidance) SettleNativeGuidance(context.Context, ports.NativeGuidanceSettlement) error {
	return nil
}

type sandboxIngress struct{ endpoint string }

func (i sandboxIngress) RegisterCallback(_ context.Context, r ports.CallbackRegistration) (ports.CallbackBinding, error) {
	return ports.CallbackBinding{RegistrationID: r.RegistrationID, ExecutionID: r.ExecutionID, Attempt: r.Attempt, Endpoint: i.endpoint, Route: "/callback", Cleanup: sandboxCallbackCleanup{r}}, nil
}

type sandboxCallbackCleanup struct{ registration ports.CallbackRegistration }

func (c sandboxCallbackCleanup) RegistrationID() string           { return c.registration.RegistrationID }
func (c sandboxCallbackCleanup) ExecutionID() model.ExecutionID   { return c.registration.ExecutionID }
func (c sandboxCallbackCleanup) Attempt() model.AttemptGeneration { return c.registration.Attempt }
func (sandboxCallbackCleanup) Close(context.Context) error        { return nil }

func sandboxForkHistory(t *testing.T, provider *Provider, workspace string) *ports.HistorySourceSelection {
	t.Helper()
	transcript := filepath.Join(provider.nativeHome, "sessions", "rollout-fixture.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0700))
	data := "{\"timestamp\":\"2026-09-06T10:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"00000000-0000-4000-8000-000000000001\",\"cwd\":" + strconv.Quote(workspace) + "}}\n" +
		"{\"timestamp\":\"2026-09-06T10:01:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-7\"}}\n"
	require.NoError(t, os.WriteFile(transcript, []byte(data), 0600))
	discovered, err := provider.History().Discover(context.Background(), ports.HistoryDiscoveryRequest{Scope: ports.HistoryDiscoveryScope{WorkspaceHint: workspace}})
	require.NoError(t, err)
	require.Len(t, discovered.Histories, 1)
	item := discovered.Histories[0]
	require.Len(t, item.Points, 1)
	return &ports.HistorySourceSelection{ConversationID: "conversation", Provider: Name, Native: item.Native, SourceToken: item.SourceToken, SourceRevision: item.Coverage.SourceRevision, SourceFingerprint: item.SourceFingerprint, Point: &item.Points[0], Evidence: item.Evidence}
}
