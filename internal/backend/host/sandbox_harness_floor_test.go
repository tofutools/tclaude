//go:build linux || darwin

package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestHarnessFloorRetainsSettingsWithoutInventingMissingFiles(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "copilot"} {
		t.Run(harness, func(t *testing.T) {
			private := t.TempDir()
			require.NoError(t, os.Chmod(private, 0700))
			native := filepath.Join(private, "native")
			require.NoError(t, os.Mkdir(native, 0700))
			settings := "settings.json"
			if harness == "codex" {
				settings = "config.toml"
			}
			path := filepath.Join(native, settings)
			require.NoError(t, os.WriteFile(path, []byte("preserved bytes"), 0600))
			inspector, err := NewSandboxPathInspector([]string{private})
			require.NoError(t, err)
			planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: private})
			require.NoError(t, err)
			policy := model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate, HarnessConfig: model.SandboxHarnessConfigRead}
			selected, materialized := materializedLaunchPolicy(t, inspector, policy)
			artifact, err := planner.PrepareHarness(context.Background(), selected, materialized, ProcessSpec{Executable: "/bin/sh", Directory: native}, harness, native, SandboxProviderResource{Path: native, Access: model.SandboxFilesystemWrite})
			require.NoError(t, err)
			require.NoError(t, VerifySandboxChild(context.Background(), artifact))
			input, _, err := readSandboxChild(artifact)
			require.NoError(t, err)
			access := map[string]model.SandboxFilesystemAccess{}
			for _, pin := range input.ProviderResources {
				access[pin.Guest] = pin.Access
			}
			canonical, err := filepath.EvalSymlinks(native)
			require.NoError(t, err)
			require.Equal(t, model.SandboxFilesystemWrite, access[native])
			require.Equal(t, model.SandboxFilesystemRead, access[filepath.Join(canonical, settings)])
			require.Equal(t, model.SandboxFilesystemRead, access[filepath.Join(canonical, "hooks")])
			require.NoFileExists(t, filepath.Join(native, "mcp-config.json"))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "preserved bytes", string(data))
			// Replacing a floored file after admission invalidates the artifact.
			require.NoError(t, os.Rename(path, path+".old"))
			require.NoError(t, os.WriteFile(path, data, 0600))
			require.ErrorContains(t, VerifySandboxChild(context.Background(), artifact), "identity changed")
		})
	}
}

func TestHarnessFloorRequiresDeclaredRootAndExactReopen(t *testing.T) {
	native, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	resources := []SandboxProviderResource{{Path: native, Access: model.SandboxFilesystemWrite}}
	_, err = prepareSandboxHarnessFloor("claude", native, model.SandboxPolicy{}, nil)
	require.ErrorContains(t, err, "declared writable native root")
	floor, err := prepareSandboxHarnessFloor("claude", native, model.SandboxPolicy{HarnessConfig: model.SandboxHarnessConfigWrite}, resources)
	require.NoError(t, err)
	require.Empty(t, floor)
	require.NoDirExists(t, filepath.Join(native, "hooks"))
	policy := model.SandboxPolicy{Filesystem: []model.SandboxFilesystemRule{{HostPath: native, Access: model.SandboxFilesystemWrite}, {HostPath: filepath.Join(native, "hooks"), Access: model.SandboxFilesystemWrite}}}
	floor, err = prepareSandboxHarnessFloor("claude", native, policy, resources)
	require.NoError(t, err)
	paths := []string{}
	for _, entry := range floor {
		paths = append(paths, entry.Path)
	}
	require.NotContains(t, paths, filepath.Join(native, "hooks"))
	require.Contains(t, paths, filepath.Join(native, "skills"))
}
