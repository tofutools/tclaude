package opencode

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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type controlPolicyReader struct{ policy model.SandboxPolicy }

func (r controlPolicyReader) ReadSandboxRevision(context.Context, model.SandboxProfileRef) (model.SandboxPolicy, error) {
	return r.policy, nil
}

func TestServerRelaySandboxArtifactRetainsControlWithoutPrivateDirectoryGrant(t *testing.T) {
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
	root, err := os.MkdirTemp("/tmp", "oc-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	private, workspace := filepath.Join(root, "private"), filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Mkdir(workspace, 0700))
	secret := filepath.Join(private, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("private fixture"), 0600))
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	planner, err := host.NewSandboxLaunchPreparer(host.SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: bootstrap, Artifacts: private})
	require.NoError(t, err)
	policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{{HostPath: workspace, Access: model.SandboxFilesystemWrite}}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}}
	hash, err := sandboxpolicy.ContentHash(policy)
	require.NoError(t, err)
	materialized, err := sandboxpolicy.MaterializeScopes(context.Background(), []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: "control", RevisionID: "revision", ContentHash: hash}}}, controlPolicyReader{policy}, inspector)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	target := reservation.Addr().String()
	port := reservation.Addr().(*net.TCPAddr).Port
	require.NoError(t, reservation.Close())
	unrelated, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer unrelated.Close()
	executable, err := os.Executable()
	require.NoError(t, err)
	raw, err := json.Marshal(ServerRelayRequest{Target: target, Executable: executable, Args: []string{"-test.run=^TestServerRelayNativeFixture$"}})
	require.NoError(t, err)
	child := host.ProcessSpec{Executable: bootstrap, Args: []string{ServerRelayCommand, string(raw), host.SandboxControlFDArgument}, Directory: workspace, Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + workspace, "TMPDIR=" + workspace, "TCLAUDE_RELAY_FIXTURE_TARGET=" + target, "TCLAUDE_RELAY_FIXTURE_READY=" + filepath.Join(workspace, "ready"), "TCLAUDE_RELAY_FIXTURE_PRIVATE=" + secret, "TCLAUDE_RELAY_FIXTURE_EXTERNAL=" + unrelated.Addr().String()}}
	artifact, err := planner.PrepareControl(context.Background(), selected, materialized, child, port, host.SandboxProviderResource{Path: bootstrap, Access: model.SandboxFilesystemRead}, host.SandboxProviderResource{Path: executable, Access: model.SandboxFilesystemRead})
	require.NoError(t, err)
	require.NoFileExists(t, artifact.Path+".started")
	require.NoFileExists(t, artifact.Path+".control.json")
	command, err := artifact.Invocation(bootstrap)
	require.NoError(t, err)
	output, err := os.Create(filepath.Join(workspace, "output"))
	require.NoError(t, err)
	defer output.Close()
	command.Stdout, command.Stderr = output, output
	process, err := host.StartProcess(command)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _, _ = process.Stop(ctx, true)
		if t.Failed() {
			data, _ := os.ReadFile(output.Name())
			t.Logf("native relay output: %s", data)
		}
	})
	var control host.UnixControlIdentity
	require.Eventually(t, func() bool {
		control, err = host.ReadSandboxControl(artifact)
		_, ready := os.Stat(filepath.Join(workspace, "ready"))
		return err == nil && ready == nil
	}, 10*time.Second, 20*time.Millisecond)
	recovered, err := host.RecoverProcess(process.Identity())
	require.NoError(t, err)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return recovered.DialUnixControl(ctx, control)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodGet, "http://control/session", nil)
	require.NoError(t, err)
	request.SetBasicAuth("opencode", "disposable-secret")
	response, err := client.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "exact session", string(body))
	// The same retained input cannot create a second listener/server on retry.
	repeated, err := host.StartProcess(command)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = repeated.Stop(ctx, true)
	})
	require.Eventually(t, func() bool { return repeated.Observe().Exited }, 3*time.Second, 10*time.Millisecond)
	after, err := host.ReadSandboxControl(artifact)
	require.NoError(t, err)
	require.Equal(t, control, after)
	require.True(t, process.Observe().Running)
}
