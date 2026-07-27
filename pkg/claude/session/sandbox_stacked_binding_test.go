package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
)

func TestStackedClaudeManagedPolicyRefusesInsecureOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		key  string
	}{
		{
			name: "sandbox disabled",
			body: `{"sandbox":{"enabled":false}}`,
			key:  "enabled",
		},
		{
			name: "weaker nested sandbox",
			body: `{"sandbox":{"enableWeakerNestedSandbox":true}}`,
			key:  "enableWeakerNestedSandbox",
		},
		{
			name: "unsandboxed commands allowed",
			body: `{"sandbox":{"allowUnsandboxedCommands":true}}`,
			key:  "allowUnsandboxedCommands",
		},
		{
			name: "failure fallback allowed",
			body: `{"sandbox":{"failIfUnavailable":false}}`,
			key:  "failIfUnavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			managed := t.TempDir()
			restore := harness.SetClaudeManagedSettingsRootForTest(managed)
			t.Cleanup(restore)
			require.NoError(t, os.WriteFile(
				filepath.Join(managed, "managed-settings.json"),
				[]byte(tc.body),
				0o600,
			))

			proof, err := prepareStackedSandboxProof(
				harness.MustGet(harness.DefaultName),
				harness.NestedSandboxExecutable{
					Path:    os.Args[0],
					Version: "test",
				},
			)
			if proof != nil {
				proof.Cleanup()
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "missing capability stacked_claude_inner_policy")
			assert.Contains(t, err.Error(), "sandbox."+tc.key)
			assert.Contains(t, err.Error(), "refusing rather than falling back")
		})
	}
}

func TestStackedProofRefusesChangedEngineBeforeLaunch(t *testing.T) {
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		codexBindingTestExecutable(t),
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	data, err := os.ReadFile(proof.ManifestPath)
	require.NoError(t, err)
	var manifest stackedSandboxBindingManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.NoError(t, os.Chmod(manifest.Engine.StagePath, 0o700))
	file, err := os.OpenFile(manifest.Engine.StagePath, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.WriteString("changed")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	err = proof.Revalidate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed")
	refusal := StackedEngineBindingRefusal(harness.MustGet(harness.CodexName), err)
	assert.Contains(t, refusal.Error(), "missing capability stacked_codex_engine_binding")
	assert.Contains(t, refusal.Error(), "refusing rather than falling back")
}

func TestStackedProofRefusesReplacementEngineWithMatchingManifest(t *testing.T) {
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		codexBindingTestExecutable(t),
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	manifest := readStackedBindingManifest(t, proof.ManifestPath)
	replacement := []byte("#!/bin/sh\necho replacement\n")
	require.NoError(t, os.Chmod(manifest.Engine.StagePath, 0o700))
	require.NoError(t, os.WriteFile(manifest.Engine.StagePath, replacement, 0o500))
	manifest.Engine.Size = int64(len(replacement))
	manifest.Engine.SHA256 = stackedBindingDigest(replacement)
	writeStackedBindingManifest(t, proof.ManifestPath, manifest)

	err = proof.Revalidate()
	require.ErrorContains(t, err, "manifest changed after capability probe")
}

func codexBindingTestExecutable(t *testing.T) harness.NestedSandboxExecutable {
	t.Helper()
	root := t.TempDir()
	files := map[string]os.FileMode{
		filepath.Join("bin", "codex"):                         0o700,
		filepath.Join("bin", "codex-code-mode-host"):          0o700,
		"codex-package.json":                                  0o600,
		filepath.Join("codex-path", "rg"):                     0o700,
		filepath.Join("codex-resources", "bwrap"):             0o700,
		filepath.Join("codex-resources", "zsh", "bin", "zsh"): 0o700,
	}
	for relative, mode := range files {
		path := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(relative+"\n"), mode))
	}
	return harness.NestedSandboxExecutable{
		Path:        filepath.Join(root, "bin", "codex"),
		Version:     "test",
		RuntimeRoot: root,
	}
}

func TestStackedProofStagesOfficialCodexStandaloneSymlinkLayout(t *testing.T) {
	executable := codexBindingTestExecutable(t)
	require.NoError(t, os.Symlink(
		filepath.Join("bin", "codex"),
		filepath.Join(executable.RuntimeRoot, "codex"),
	))

	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		executable,
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	manifest := readStackedBindingManifest(t, proof.ManifestPath)
	assert.Equal(t,
		filepath.Join(stackedBoundCodexRuntimeRoot, "bin", "codex"),
		manifest.Engine.Destination,
	)
	for _, file := range manifest.RuntimeFiles {
		assert.NotEqual(t,
			filepath.Join(stackedBoundCodexRuntimeRoot, "codex"),
			file.Destination,
		)
	}
}

func TestStackedProofRefusesManifestDroppingClaudePolicyFreeze(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sealed probe helper is a Linux stacked-sandbox authority")
	}
	managed := t.TempDir()
	restore := harness.SetClaudeManagedSettingsRootForTest(managed)
	t.Cleanup(restore)
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.DefaultName),
		harness.NestedSandboxExecutable{
			Path:    os.Args[0],
			Version: "test",
		},
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	manifest := readStackedBindingManifest(t, proof.ManifestPath)
	require.True(t, manifest.FreezeClaudeManagedPolicy)
	manifest.FreezeClaudeManagedPolicy = false
	manifest.ManagedPolicy = nil
	writeStackedBindingManifest(t, proof.ManifestPath, manifest)

	err = proof.Revalidate()
	require.ErrorContains(t, err, "manifest changed after capability probe")
}

func TestStackedProofStagesRunningProbeHelperForClaudeOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sealed probe helper is a Linux stacked-sandbox authority")
	}
	managed := t.TempDir()
	restore := harness.SetClaudeManagedSettingsRootForTest(managed)
	t.Cleanup(restore)
	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.DefaultName),
		harness.NestedSandboxExecutable{
			Path:    os.Args[0],
			Version: "test",
		},
	)
	require.NoError(t, err)
	t.Cleanup(proof.Cleanup)

	manifest := readStackedBindingManifest(t, proof.ManifestPath)
	require.NotNil(t, manifest.ProbeHelper)
	assert.Equal(t, probehelper.BoundPath, manifest.ProbeHelper.Destination)
	assert.Equal(t, uint32(0o500), manifest.ProbeHelper.Mode)
	assert.NotEqual(t, os.Args[0], manifest.ProbeHelper.StagePath)
	require.NoError(t, proof.Revalidate())
	helperStagePath := manifest.ProbeHelper.StagePath
	require.NoError(t, proof.completeProbe())
	finalManifest := readStackedBindingManifest(t, proof.ManifestPath)
	assert.Nil(t, finalManifest.ProbeHelper)
	_, err = os.Stat(helperStagePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, proof.Revalidate())

	codexProof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.CodexName),
		codexBindingTestExecutable(t),
	)
	require.NoError(t, err)
	t.Cleanup(codexProof.Cleanup)
	codexManifest := readStackedBindingManifest(t, codexProof.ManifestPath)
	assert.Nil(t, codexManifest.ProbeHelper)
}

func TestStackedProofNamesProbeHelperStagingFailure(t *testing.T) {
	previous := stackedProbeHelperSourcePath
	stackedProbeHelperSourcePath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { stackedProbeHelperSourcePath = previous })
	managed := t.TempDir()
	restore := harness.SetClaudeManagedSettingsRootForTest(managed)
	t.Cleanup(restore)

	proof, err := prepareStackedSandboxProof(
		harness.MustGet(harness.DefaultName),
		harness.NestedSandboxExecutable{
			Path:    os.Args[0],
			Version: "test",
		},
	)
	if proof != nil {
		proof.Cleanup()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing capability stacked_claude_probe_helper")
	assert.Contains(t, err.Error(), "running Go probe helper")
	assert.Contains(t, err.Error(), "refusing rather than falling back")
}

func readStackedBindingManifest(
	t *testing.T,
	path string,
) stackedSandboxBindingManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var manifest stackedSandboxBindingManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	return manifest
}

func writeStackedBindingManifest(
	t *testing.T,
	path string,
	manifest stackedSandboxBindingManifest,
) {
	t.Helper()
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
