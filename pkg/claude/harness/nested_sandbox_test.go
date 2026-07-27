package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNestedSandboxContractsPrepareRealInnerEngines(t *testing.T) {
	claude := MustGet(DefaultName)
	require.True(t, claude.SupportsNestedSandbox())
	claudeSpec := claude.NestedSandbox.PrepareLaunch(SpawnSpec{
		SandboxMode: ClaudeSandboxOff,
	})
	assert.Equal(t, ClaudeSandboxOn, claudeSpec.SandboxMode)
	assert.True(t, claudeSpec.StrongNestedSandbox)
	var settings map[string]any
	require.NoError(t, json.Unmarshal([]byte(claudeSettingsJSON(claudeSpec)), &settings))
	block := settings["sandbox"].(map[string]any)
	assert.Equal(t, false, block["enableWeakerNestedSandbox"])
	assert.Equal(t, true, block["enabled"])

	codex := MustGet(CodexName)
	require.True(t, codex.SupportsNestedSandbox())
	codexSpec := codex.NestedSandbox.PrepareLaunch(SpawnSpec{
		SandboxMode: SandboxDangerFull,
	})
	assert.Empty(t, codexSpec.SandboxMode)
	assert.Equal(t, CodexAgentProfile, codexSpec.PermissionProfile)
	assert.True(t, codexSpec.StrongNestedSandbox)
	command := codex.Spawn.BuildCommand(codexSpec)
	assert.Contains(t, command, "-p "+CodexAgentProfile)
	assert.Contains(t, command, "features.use_legacy_landlock=false")
}

func TestCodexNestedSandboxResolvesNPMNativeBackend(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Codex stacked sandbox is Linux-only")
	}
	packageName, targetTriple, err := codexLinuxNativeTarget()
	require.NoError(t, err)
	root := t.TempDir()
	launcher := filepath.Join(
		root,
		"node_modules",
		"@openai",
		"codex",
		"bin",
		"codex.js",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o700))
	require.NoError(t, os.WriteFile(
		launcher,
		[]byte("#!/usr/bin/env node\n"),
		0o700,
	))
	packageParts := strings.Split(packageName, "/")
	native := filepath.Join(
		root,
		"node_modules",
		packageParts[0],
		packageParts[1],
		"vendor",
		targetTriple,
		"bin",
		"codex",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(native), 0o700))
	require.NoError(t, os.WriteFile(
		native,
		[]byte("#!/bin/sh\necho codex-cli test-native\n"),
		0o700,
	))
	nativeRoot := filepath.Dir(filepath.Dir(native))
	for _, relative := range []string{
		"codex-package.json",
		filepath.Join("codex-path", "rg"),
		filepath.Join("codex-resources", "bwrap"),
	} {
		path := filepath.Join(nativeRoot, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(relative), 0o700))
	}

	resolved, err := resolveCodexNativeExecutable(
		context.Background(),
		NestedSandboxExecutable{Path: launcher, Version: "launcher"},
	)
	require.NoError(t, err)
	assert.Equal(t, native, resolved.Path)
	assert.Equal(t, "codex-cli test-native", resolved.Version)
	assert.Equal(t, nativeRoot, resolved.RuntimeRoot)
}

func TestNestedSandboxProbeCommandsAreModelFreeAndPolicyShaped(t *testing.T) {
	executable := NestedSandboxExecutable{Path: "/usr/bin/engine"}
	for _, tc := range []struct {
		name     string
		contract NestedSandboxContract
		contains []string
	}{
		{
			name: "claude", contract: claudeNestedSandbox{},
			contains: []string{
				"--settings",
				"enableWeakerNestedSandbox",
				"socket.AF_UNIX",
				"env -i",
				"ANTHROPIC_BASE_URL=",
				"--bare --safe-mode",
				"TCLAUDE_STACKED_STUB_OK_",
			},
		},
		{
			name: "codex", contract: codexNestedSandbox{},
			contains: []string{"sandbox -P stacked-probe", "use_legacy_landlock = false", "CODEX_HOME="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "codex" {
				t.Setenv("CODEX_HOME", t.TempDir())
			}
			probe, err := tc.contract.PrepareProbe(t.TempDir(), executable)
			require.NoError(t, err)
			t.Cleanup(probe.Cleanup)
			joined := probe.Command
			for _, path := range probe.KnownPaths {
				if strings.HasSuffix(path, "settings.json") || strings.HasSuffix(path, "config.toml") {
					content, err := os.ReadFile(path)
					require.NoError(t, err)
					joined += string(content)
				}
			}
			for _, expected := range tc.contains {
				assert.Contains(t, joined, expected)
			}
			assert.Contains(t, joined, string(os.PathSeparator)+"workspace"+string(os.PathSeparator)+"allowed")
			assert.Contains(t, joined, string(os.PathSeparator)+"private"+string(os.PathSeparator)+"denied")
			assert.NotContains(t, joined, "prompt")
		})
	}
}

func TestCodexNestedProbeKeepsStateHomeOutsideTemporaryWorkspace(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("CODEX_HOME", stateRoot)
	workspaceRoot := t.TempDir()
	probe, err := (codexNestedSandbox{}).PrepareProbe(
		workspaceRoot,
		NestedSandboxExecutable{Path: "/usr/bin/codex"},
	)
	require.NoError(t, err)
	t.Cleanup(probe.Cleanup)
	var configPath string
	for _, path := range probe.KnownPaths {
		if filepath.Base(path) == "config.toml" {
			configPath = path
			break
		}
	}
	require.NotEmpty(t, configPath)
	assert.True(t, strings.HasPrefix(configPath, stateRoot+string(os.PathSeparator)))
	assert.False(t, strings.HasPrefix(configPath, workspaceRoot+string(os.PathSeparator)))
}
