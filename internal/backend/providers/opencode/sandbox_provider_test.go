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
	binary, err := os.Executable()
	require.NoError(t, err)
	native := filepath.Join(private, "native-fixture")
	script := `#!/bin/sh
if [ "$1" = serve ]; then
  if cat "$PRIVATE_FIXTURE" >/dev/null 2>&1; then exit 81; fi
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
	provider, err := New(Config{Executable: native, PrivateRoot: filepath.Join(private, "provider"), HostSandbox: planner,
		Environment: []string{"OPENCODE_TEST_BINARY=" + binary, "OPENCODE_TEST_PROMPT=" + prompt, "PRIVATE_FIXTURE=" + secret, "ENVIRONMENT_OUTPUT=" + filepath.Join(workspace, "environment")}})
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
}
