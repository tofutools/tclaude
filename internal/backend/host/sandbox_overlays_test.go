//go:build linux || darwin

package host

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxDescriptorOverlaysNative(t *testing.T) {
	name := "bwrap"
	if runtime.GOOS == "darwin" {
		name = "sandbox-exec"
	}
	wrapper, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") == "1" {
			t.Fatal(err)
		}
		t.Skip("native wrapper unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	private, work := filepath.Join(root, "private"), filepath.Join(root, "work")
	hidden, keep := filepath.Join(work, "hidden"), filepath.Join(work, "hidden", "keep")
	scratch, exported := filepath.Join(work, "scratch"), filepath.Join(root, "exported")
	for _, path := range []string{private, keep, scratch, exported} {
		require.NoError(t, os.MkdirAll(path, 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(hidden, "secret"), []byte("not exposed"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "host-file"), []byte("host scratch"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(exported, "visible"), []byte("explicit nested bind"), 0600))
	binary, err := os.Executable()
	require.NoError(t, err)
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{
		{HostPath: binary, Access: model.SandboxFilesystemRead},
		{HostPath: work, Access: model.SandboxFilesystemWrite},
		{HostPath: hidden, Access: model.SandboxFilesystemDeny},
		{HostPath: keep, Access: model.SandboxFilesystemWrite},
	}}
	if runtime.GOOS == "linux" {
		policy.Tmpfs = []model.SandboxTmpfs{{GuestPath: scratch, Size: "1MiB"}}
		policy.Filesystem = append(policy.Filesystem, model.SandboxFilesystemRule{HostPath: exported, GuestPath: filepath.Join(scratch, "export"), Access: model.SandboxFilesystemRead})
	}
	selected, materialized := materializedLaunchPolicy(t, inspector, policy)
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: binary, Artifacts: private})
	require.NoError(t, err)
	artifact, err := planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: binary, Args: []string{"-test.run=^TestSandboxDescriptorOverlaysChild$"}, Directory: work, Env: []string{"TCLAUDE_OVERLAY_CHILD=1", "WORK=" + work}, ExactEnvironment: true})
	require.NoError(t, err)
	require.NoError(t, VerifySandboxChild(context.Background(), artifact))
	var output bytes.Buffer
	process, err := StartProcess(ProcessSpec{Executable: binary, Args: []string{"-test.run=^TestSandboxBootstrapHelper$"}, ExactEnvironment: true, Env: []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}, Stdout: &output, Stderr: &output})
	require.NoError(t, err)
	require.Eventually(t, func() bool { row := process.Observe(); return row.Exited && row.ExitCode != nil }, 10*time.Second, 10*time.Millisecond)
	result := process.Observe()
	if *result.ExitCode != 0 && os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") != "1" {
		for _, message := range []string{"No permissions to create a new namespace", "Creating new namespace failed: Operation not permitted", "loopback: Failed RTM_NEWADDR: Operation not permitted"} {
			if strings.Contains(output.String(), message) {
				t.Skipf("host refuses native namespaces: %s", output.String())
			}
		}
	}
	require.Zero(t, *result.ExitCode, output.String())
	require.FileExists(t, filepath.Join(keep, "written"))
	require.NoFileExists(t, filepath.Join(hidden, "forbidden-write"))
	require.NoFileExists(t, filepath.Join(scratch, "ephemeral"))
	content, err := os.ReadFile(filepath.Join(scratch, "host-file"))
	require.NoError(t, err)
	require.Equal(t, "host scratch", string(content))
}

func TestSandboxDescriptorOverlaysChild(t *testing.T) {
	if os.Getenv("TCLAUDE_OVERLAY_CHILD") != "1" {
		t.Skip("native child only")
	}
	work := os.Getenv("WORK")
	_, err := os.ReadFile(filepath.Join(work, "hidden", "secret"))
	require.Error(t, err)
	require.Error(t, os.WriteFile(filepath.Join(work, "hidden", "forbidden-write"), []byte("no"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(work, "hidden", "keep", "written"), []byte("yes"), 0600))
	if runtime.GOOS == "linux" {
		scratch := filepath.Join(work, "scratch")
		_, err := os.ReadFile(filepath.Join(scratch, "host-file"))
		require.Error(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(scratch, "ephemeral"), []byte("temporary"), 0600))
		content, err := os.ReadFile(filepath.Join(scratch, "export", "visible"))
		require.NoError(t, err)
		require.Equal(t, "explicit nested bind", string(content))
		require.Error(t, os.WriteFile(filepath.Join(scratch, "export", "forbidden"), []byte("no"), 0600))
		require.Error(t, os.WriteFile(filepath.Join(scratch, "too-large"), make([]byte, 2<<20), 0600), "authored scratch size must be enforced")
	}
}

func TestSandboxOverlaysRefuseChangedDenyAliasAndShadowedResources(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	private, work := filepath.Join(root, "private"), filepath.Join(root, "work")
	for _, path := range []string{private, work, filepath.Join(work, "first"), filepath.Join(work, "second")} {
		require.NoError(t, os.MkdirAll(path, 0700))
	}
	alias := filepath.Join(work, "alias")
	require.NoError(t, os.Symlink(filepath.Join(work, "first"), alias))
	i, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	bound := &SandboxMountBindings{}
	overlays, err := i.prepareSandboxOverlays(context.Background(), []model.SandboxFilesystemRule{{HostPath: alias, Access: model.SandboxFilesystemDeny}}, nil, bound, "/bin/sh", work)
	require.NoError(t, err)
	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(filepath.Join(work, "second"), alias))
	require.ErrorContains(t, i.reopenSandboxOverlays(context.Background(), bound, overlays, "/bin/sh", work), "path changed")
	_, err = i.prepareSandboxOverlays(context.Background(), nil, []model.SandboxTmpfs{{GuestPath: work}}, bound, "/bin/sh", work)
	if runtime.GOOS == "darwin" {
		require.ErrorContains(t, err, "Seatbelt cannot")
	} else {
		require.ErrorContains(t, err, "launch-required")
	}
}
