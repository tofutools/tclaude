//go:build linux || darwin

package host

import (
	"bytes"
	"context"
	"fmt"
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

func TestSandboxDescriptorOverlaysNative(t *testing.T) { sandboxOverlaysAndDirectoriesNative(t, false) }
func TestSandboxDescriptorIndividualDirectoriesNative(t *testing.T) {
	sandboxOverlaysAndDirectoriesNative(t, true)
}

func sandboxOverlaysAndDirectoriesNative(t *testing.T, individual bool) {
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
	policy.AgentDirectories = []string{"BUILD_CACHE"}
	policy.PreLaunch = []model.SandboxSetupBlock{
		{Name: "prepare", Script: `if cat "$WORK/hidden/secret" >/dev/null 2>&1; then exit 90; fi
printf 'prepared' > "$WORK/setup-marker"
if [ "$INDIVIDUAL" = false ]; then rmdir "$BUILD_CACHE"; mkdir "$BUILD_CACHE";
else if rmdir "$BUILD_CACHE" 2>/dev/null; then exit 91; fi; fi
printf 'cached' > "$BUILD_CACHE/prepared"
setup_value() { printf 'ordered'; }
export SETUP_VALUE=$(setup_value)`, Exports: []string{"SETUP_VALUE"}},
		{Name: "finish", Script: `export SETUP_VALUE="$SETUP_VALUE:$(setup_value)"; set -- must-not-replace-native-command`, Exports: []string{"SETUP_VALUE"}},
	}
	selected, materialized := materializedLaunchPolicy(t, inspector, policy)
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: binary, Artifacts: private, AgentDirectoriesMountIndividually: individual})
	require.NoError(t, err)
	actor := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "first", ExecutionID: "one"})
	command := ProcessSpec{Executable: binary, Args: []string{"-test.run=^TestSandboxDescriptorOverlaysChild$"}, Directory: work, Env: []string{"TCLAUDE_OVERLAY_CHILD=1", "WORK=" + work, "BUILD_CACHE=must-not-win", "INDIVIDUAL=" + fmt.Sprint(individual)}, ExactEnvironment: true}
	artifact, err := actor.Prepare(context.Background(), selected, materialized, command)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(work, "setup-marker"))
	require.NoError(t, VerifySandboxChild(context.Background(), artifact))
	var output bytes.Buffer
	process, err := StartProcess(ProcessSpec{Executable: binary, Args: []string{"-test.run=^TestSandboxBootstrapHelper$"}, ExactEnvironment: true, Env: []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}, Stdout: &output, Stderr: &output})
	require.NoError(t, err)
	require.Eventually(t, func() bool { row := process.Observe(); return row.Exited && row.ExitCode != nil }, 10*time.Second, 10*time.Millisecond)
	result := process.Observe()
	if *result.ExitCode != 0 && os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") != "1" {
		if sandboxNamespaceUnavailable(output.String()) {
			t.Skipf("host refuses native namespaces: %s", output.String())
		}
	}
	require.Zero(t, *result.ExitCode, output.String())
	require.FileExists(t, filepath.Join(keep, "written"))
	require.NoFileExists(t, filepath.Join(hidden, "forbidden-write"))
	require.NoFileExists(t, filepath.Join(scratch, "ephemeral"))
	content, err := os.ReadFile(filepath.Join(scratch, "host-file"))
	require.NoError(t, err)
	require.Equal(t, "host scratch", string(content))
	input, _, err := readSandboxChild(artifact)
	require.NoError(t, err)
	cache := ""
	for _, entry := range input.Environment {
		if strings.HasPrefix(entry, "BUILD_CACHE=") {
			cache = strings.TrimPrefix(entry, "BUILD_CACHE=")
		}
	}
	require.FileExists(t, filepath.Join(cache, "prepared"))
	continued, err := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "first", ExecutionID: "two"}).Prepare(context.Background(), selected, materialized, command)
	require.NoError(t, err)
	continuedInput, _, err := readSandboxChild(continued)
	require.NoError(t, err)
	require.Contains(t, continuedInput.Environment, "BUILD_CACHE="+cache)
	fresh, err := planner.ForExecution(model.ResolvedExecutionSpec{AgentID: "second", ExecutionID: "three"}).Prepare(context.Background(), selected, materialized, command)
	require.NoError(t, err)
	freshInput, _, err := readSandboxChild(fresh)
	require.NoError(t, err)
	require.NotContains(t, freshInput.Environment, "BUILD_CACHE="+cache)
	for _, entry := range freshInput.Environment {
		if strings.HasPrefix(entry, "BUILD_CACHE=") {
			require.NoFileExists(t, filepath.Join(strings.TrimPrefix(entry, "BUILD_CACHE="), "prepared"))
		}
	}
}

func TestSandboxDescriptorOverlaysChild(t *testing.T) {
	if os.Getenv("TCLAUDE_OVERLAY_CHILD") != "1" {
		t.Skip("native child only")
	}
	require.FileExists(t, filepath.Join(os.Getenv("BUILD_CACHE"), "prepared"))
	require.Equal(t, "ordered:ordered", os.Getenv("SETUP_VALUE"))
	work := os.Getenv("WORK")
	require.FileExists(t, filepath.Join(work, "setup-marker"))
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
