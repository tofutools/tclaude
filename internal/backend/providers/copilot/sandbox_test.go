//go:build linux || darwin

package copilot

import (
	"context"
	"encoding/json"
	"net"
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

type sandboxPolicyReader struct{ policy model.SandboxPolicy }

func (r sandboxPolicyReader) ReadSandboxRevision(context.Context, model.SandboxProfileRef) (model.SandboxPolicy, error) {
	return r.policy, nil
}

func TestProviderHostSandboxPreparesExactCommandAndRefusesChangedCredentialBeforeRelease(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("/tmp", "sb-copilot-")
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
	request := ports.PreparationRequest{HostSandboxPolicy: &materialized, Spec: model.ResolvedExecutionSpec{HostSandbox: &selected, ExecutionID: "execution", Attempt: 1, Harness: Name, Model: "fixture", WorkingDirectory: workspace, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}, Intent: ports.StartFresh, InitialInput: &ports.PreparedInitialInput{Body: "Exact first work", Correlation: "brief", RequiredBeforeFirstWork: true}, ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "execution", Generation: 1, DeliveryID: "delivery", Secret: []byte("disposable action credential"), ExpiresAt: time.Now().Add(time.Hour)}}
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
	require.Contains(t, command.Environment, "COPILOT_HOME="+provider.nativeHome)
	require.NotContains(t, string(data), "must not be copied")
	require.NotContains(t, string(data), "disposable action credential")
	require.Len(t, command.ProviderResources, 4)
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
}

// A synthetic native executable exercises the production provider, terminal,
// shipped bootstrap and kernel wrapper. It performs no authenticated native
// harness work and uses only disposable credentials and directories.
func TestProviderHostSandboxNativeLaunchAndRecovery(t *testing.T) {
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
	root, err := os.MkdirTemp("/tmp", "cp-native-")
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
	executable := filepath.Join(workspace, "native-fixture")
	script := `#!/bin/sh
set -eu
test "$(cat "$TCLAUDE_BACKEND_CREDENTIAL_FILE")" = 'fixture credential'
if cat "$PRIVATE_SIBLING" >/dev/null 2>&1; then exit 24; fi
printf '%s\n' "$@" > "$WORKSPACE/args"
printf ready > "$WORKSPACE/ready"
while IFS= read -r line; do printf '%s\n' "$line" >> "$WORKSPACE/input"; done
`
	require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
	socket := filepath.Join(private, "api.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	planner, err := host.NewSandboxLaunchPreparer(host.SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: bootstrap, Artifacts: artifacts})
	require.NoError(t, err)
	provider, err := New(Config{Executable: executable, PrivateRoot: filepath.Join(private, "provider"), AgentSocket: socket, HostSandbox: planner})
	require.NoError(t, err)
	policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{{HostPath: workspace, Access: model.SandboxFilesystemWrite}}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}}
	hash, err := sandboxpolicy.ContentHash(policy)
	require.NoError(t, err)
	ref := model.SandboxProfileRef{ProfileID: "policy", RevisionID: "revision", ContentHash: hash}
	materialized, err := sandboxpolicy.MaterializeScopes(context.Background(), []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: ref}}, sandboxPolicyReader{policy}, inspector)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	request := ports.PreparationRequest{HostSandboxPolicy: &materialized, Spec: model.ResolvedExecutionSpec{HostSandbox: &selected, ExecutionID: "native_execution", Attempt: 1, Harness: Name, Model: "fixture", WorkingDirectory: workspace, Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined, Environment: model.Environment{"PRIVATE_SIBLING": sibling, "WORKSPACE": workspace}}, Intent: ports.StartFresh, InitialInput: &ports.PreparedInitialInput{Body: "first work literal", Correlation: "brief", RequiredBeforeFirstWork: true}, ActionCredential: &ports.ActionCredentialMaterial{ExecutionID: "native_execution", Generation: 1, DeliveryID: "delivery", Secret: []byte("fixture credential"), ExpiresAt: time.Now().Add(time.Hour)}}
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
	args, err := os.ReadFile(filepath.Join(workspace, "args"))
	require.NoError(t, err)
	require.Contains(t, string(args), "first work literal")
	recorded, err := decodeEvidence(released.Evidence)
	require.NoError(t, err)
	require.NotNil(t, recorded.HostSandbox)
	require.Equal(t, selected.PolicyHash, recorded.HostSandboxPolicyHash)
	require.FileExists(t, recorded.HostSandbox.Path+".started")
	recovered, err := provider.Recover(context.Background(), ports.RecoveryRequest{ExecutionID: request.Spec.ExecutionID, Spec: request.Spec, Attempt: 1, Evidence: released.Evidence})
	require.NoError(t, err)
	require.Equal(t, ports.RecoveryControlled, recovered.State)
	evidence, err := recovered.Runtime.(*Runtime).providerEvidence()
	require.NoError(t, err)
	retained, err := decodeEvidence(evidence)
	require.NoError(t, err)
	require.Equal(t, recorded.HostSandbox, retained.HostSandbox)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = recovered.Runtime.Stop(ctx, ports.StopRequest{Force: true})
	require.NoError(t, err)
}
