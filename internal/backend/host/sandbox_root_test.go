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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxRootSelectionRetainsNetworkMinimum(t *testing.T) {
	for _, mode := range []model.SandboxFilesystemRoot{model.SandboxRootAutomatic, model.SandboxRootInherit, model.SandboxRootSeparate} {
		for _, isolated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", mode, isolated), func(t *testing.T) {
				planner, _ := directoryPlanner(t, false)
				work := t.TempDir()
				policy := model.SandboxPolicy{FilesystemRoot: mode, Filesystem: []model.SandboxFilesystemRule{{HostPath: work, Access: model.SandboxFilesystemWrite}}}
				if isolated {
					policy.Network = &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}
				}
				selected, materialized := materializedLaunchPolicy(t, planner.config.Inspector, policy)
				artifact, err := planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Directory: work})
				require.NoError(t, err)
				input, _, err := readSandboxChild(artifact)
				require.NoError(t, err)
				require.Equal(t, mode != model.SandboxRootSeparate && !isolated, input.InheritedRoot)
				require.NoError(t, VerifySandboxChild(context.Background(), artifact))
			})
		}
	}
}

func TestSandboxDescriptorInheritedRootNative(t *testing.T) {
	wrapperName := "bwrap"
	if runtime.GOOS == "darwin" {
		wrapperName = "sandbox-exec"
	}
	wrapper, err := exec.LookPath(wrapperName)
	if err != nil {
		if os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") == "1" {
			t.Fatal(err)
		}
		t.Skip("native wrapper unavailable")
	}
	for _, mode := range []model.SandboxFilesystemRoot{model.SandboxRootInherit, model.SandboxRootSeparate} {
		t.Run(string(mode), func(t *testing.T) {
			// Outside /tmp: Linux intentionally replaces host scratch in both modes.
			// A root-owned CI namespace cannot traverse the runner-owned checkout
			// using host capabilities, so keep its fixture under public /var/tmp.
			parent := os.Getenv("TCLAUDE_SANDBOX_ROOT_FIXTURE_PARENT")
			if parent == "" {
				parent = "."
			}
			path, err := os.MkdirTemp(parent, ".sandbox-root-")
			require.NoError(t, err)
			defer func() { require.NoError(t, os.RemoveAll(path)) }()
			root, err := filepath.Abs(path)
			require.NoError(t, err)
			root, err = filepath.EvalSymlinks(root)
			require.NoError(t, err)
			private, work, denied := filepath.Join(root, "private"), filepath.Join(root, "work"), filepath.Join(root, "denied")
			for _, directory := range []string{private, work, denied} {
				require.NoError(t, os.Mkdir(directory, 0700))
			}
			for _, file := range []string{filepath.Join(root, "ambient"), filepath.Join(private, "secret"), filepath.Join(private, "allowed"), filepath.Join(denied, "secret")} {
				require.NoError(t, os.WriteFile(file, []byte("retained"), 0600))
			}
			binary, err := os.Executable()
			require.NoError(t, err)
			inspector, err := NewSandboxPathInspector([]string{private})
			require.NoError(t, err)
			selected, materialized := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{FilesystemRoot: mode, Filesystem: []model.SandboxFilesystemRule{{HostPath: work, Access: model.SandboxFilesystemWrite}, {HostPath: denied, Access: model.SandboxFilesystemDeny}}})
			planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: binary, Artifacts: private})
			require.NoError(t, err)
			artifact, err := planner.Prepare(context.Background(), selected, materialized, ProcessSpec{Executable: binary, Args: []string{"-test.run=^TestSandboxDescriptorInheritedRootChild$"}, Directory: work, Env: []string{"TCLAUDE_ROOT_CHILD=1", "ROOT=" + root, "INHERITED=" + fmt.Sprint(mode == model.SandboxRootInherit)}}, SandboxProviderResource{Path: binary, Access: model.SandboxFilesystemRead}, SandboxProviderResource{Path: filepath.Join(private, "allowed"), Access: model.SandboxFilesystemRead})
			require.NoError(t, err)
			require.NoError(t, VerifySandboxChild(context.Background(), artifact))
			var output bytes.Buffer
			process, err := StartProcess(ProcessSpec{Executable: binary, Args: []string{"-test.run=^TestSandboxBootstrapHelper$"}, Env: []string{"TCLAUDE_BOOTSTRAP_ARTIFACT=" + artifact.Path, "TCLAUDE_BOOTSTRAP_DIGEST=" + artifact.Digest}, ExactEnvironment: true, Stdout: &output, Stderr: &output})
			require.NoError(t, err)
			require.Eventually(t, func() bool { row := process.Observe(); return row.Exited && row.ExitCode != nil }, 10*time.Second, 10*time.Millisecond)
			result := process.Observe()
			if *result.ExitCode != 0 && os.Getenv("TCLAUDE_REQUIRE_SANDBOX_NATIVE") != "1" {
				if sandboxNamespaceUnavailable(output.String()) {
					t.Skipf("native namespaces unavailable: %s", output.String())
				}
			}
			require.Zero(t, *result.ExitCode, output.String())
			require.FileExists(t, filepath.Join(work, "written"))
			require.NoFileExists(t, filepath.Join(private, "forbidden"))
		})
	}
}

func TestSandboxDescriptorInheritedRootChild(t *testing.T) {
	if os.Getenv("TCLAUDE_ROOT_CHILD") != "1" {
		t.Skip("native child only")
	}
	root := os.Getenv("ROOT")
	_, err := os.ReadFile(filepath.Join(root, "ambient"))
	if os.Getenv("INHERITED") == "true" {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
	require.Error(t, os.WriteFile(filepath.Join(root, "ambient"), []byte("no"), 0600))
	_, err = os.ReadFile(filepath.Join(root, "private", "secret"))
	require.Error(t, err)
	require.Error(t, os.WriteFile(filepath.Join(root, "private", "forbidden"), []byte("no"), 0600))
	_, err = os.ReadFile(filepath.Join(root, "denied", "secret"))
	require.Error(t, err)
	allowed, err := os.ReadFile(filepath.Join(root, "private", "allowed"))
	require.NoError(t, err)
	require.Equal(t, "retained", string(allowed))
	require.NoError(t, os.WriteFile(filepath.Join(root, "work", "written"), []byte("yes"), 0600))
}
