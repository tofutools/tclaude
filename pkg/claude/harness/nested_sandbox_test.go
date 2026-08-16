package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
)

func TestNestedSandboxContractsPrepareRealInnerEngines(t *testing.T) {
	claude := MustGet(DefaultName)
	require.True(t, claude.SupportsNestedSandbox())
	claudeSpec := claude.NestedSandbox.PrepareLaunch(SpawnSpec{
		HarnessBuiltinMode: ClaudeSandboxOff,
	})
	assert.Equal(t, ClaudeSandboxOn, claudeSpec.HarnessBuiltinMode)
	assert.True(t, claudeSpec.StrongNestedSandbox)
	var settings map[string]any
	require.NoError(t, json.Unmarshal([]byte(claudeSettingsJSON(claudeSpec)), &settings))
	block, ok := settings["sandbox"].(map[string]any)
	require.True(t, ok, "Claude nested launch settings must carry a sandbox block")
	assert.Equal(t, false, block["enableWeakerNestedSandbox"])
	assert.Equal(t, true, block["enabled"])

	codex := MustGet(CodexName)
	require.True(t, codex.SupportsNestedSandbox())
	codexSpec := codex.NestedSandbox.PrepareLaunch(SpawnSpec{
		HarnessBuiltinMode: SandboxDangerFull,
	})
	assert.Empty(t, codexSpec.HarnessBuiltinMode)
	assert.Equal(t, CodexAgentProfile, codexSpec.PermissionProfile)
	assert.True(t, codexSpec.StrongNestedSandbox)
	command := codex.Spawn.BuildCommand(codexSpec)
	assert.Contains(t, command, "-p "+CodexAgentProfile)
	assert.Contains(t, command, "features.use_legacy_landlock=false")
}

func TestNestedSandboxCapabilityHandlesNilError(t *testing.T) {
	capability, detail := NestedSandboxCapability(nil, "stacked_test")
	assert.Equal(t, "stacked_test", capability)
	assert.Empty(t, detail)
}

func TestClaudeAFUnixSeccompProbeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		helperExit *int
		wantExit   int
	}{
		{name: "missing_helper", wantExit: 96},
		{name: "expected_seccomp_refusal", helperExit: intPointer(77), wantExit: 0},
		{name: "socket_allowed", helperExit: intPointer(0), wantExit: 92},
		{name: "untestable", helperExit: intPointer(78), wantExit: 96},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helperPath := filepath.Join(t.TempDir(), "probe-helper")
			if tc.helperExit != nil {
				script := []byte(
					"#!/bin/sh\nexit " + strconv.Itoa(*tc.helperExit) + "\n",
				)
				require.NoError(t, os.WriteFile(helperPath, script, 0o700))
			}
			cmd := exec.Command(
				"/bin/sh",
				"-c",
				claudeAFUnixSeccompProbeScript(helperPath),
			)
			err := cmd.Run()
			if tc.wantExit == 0 {
				require.NoError(t, err)
				return
			}
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			require.Equal(t, tc.wantExit, exitErr.ExitCode())
		})
	}
}

func intPointer(value int) *int {
	return &value
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
		"stacked_codex_bwrap_backend",
	)
	require.NoError(t, err)
	assert.Equal(t, native, resolved.Path)
	assert.Equal(t, "codex-cli test-native", resolved.Version)
	assert.Equal(t, nativeRoot, resolved.RuntimeRoot)
}

func TestCodexNPMNativeBackendDoesNotSearchAboveLauncherPackage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Codex stacked sandbox is Linux-only")
	}
	packageName, targetTriple, err := codexLinuxNativeTarget()
	require.NoError(t, err)
	root := t.TempDir()
	launcher := filepath.Join(root, "tools", "codex.js")
	require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o700))
	require.NoError(t, os.WriteFile(launcher, []byte("launcher"), 0o600))
	packageParts := strings.Split(packageName, "/")
	attackerBackend := filepath.Join(
		root,
		"node_modules",
		packageParts[0],
		packageParts[1],
		"vendor",
		targetTriple,
		"bin",
		"codex",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(attackerBackend), 0o700))
	require.NoError(t, os.WriteFile(attackerBackend, []byte("backend"), 0o700))

	_, err = findCodexNPMNativeBackend(launcher, packageName, targetTriple)
	require.ErrorContains(t, err, "outside a recognized node_modules/@openai/codex package")
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
				"env -i",
				probehelper.StubMode,
				probehelper.AFUnixMode,
				probehelper.BoundPath,
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
			if tc.name == "claude" {
				assert.Contains(t, joined, "socket_status")
				assert.Contains(t, joined, "AF_UNIX probe was untestable")
				assert.NotContains(t, joined, "python3 ")
				assert.NotContains(t, joined, ".py")
				assert.GreaterOrEqual(t, strings.Count(joined, "env -i"), 3)
				require.NotNil(t, probe.ClassifyFailure)
				assert.Empty(t, probe.ClassifyFailure("ordinary CLI failure"))
				assert.Equal(
					t,
					"stacked_claude_probe_helper",
					probe.ClassifyFailure(probehelper.BindingFailureMarker),
				)
				settingsPath := probe.KnownPaths[1]
				require.NoError(t, os.WriteFile(
					filepath.Join(
						filepath.Dir(settingsPath),
						probehelper.InnerPolicyFileName,
					),
					[]byte(probehelper.InnerPolicyFailureValue),
					0o600,
				))
				assert.Equal(
					t,
					"stacked_claude_inner_policy",
					probe.ClassifyFailure(""),
				)
			}
		})
	}
}

func TestClaudeNestedSandboxProbeIgnoresUntrustedPathEnv(t *testing.T) {
	fakeBin := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(fakeBin, "env"),
		[]byte("#!/bin/sh\nexit 77\n"),
		0o700,
	))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	probe, err := (claudeNestedSandbox{}).PrepareProbe(
		t.TempDir(),
		NestedSandboxExecutable{Path: "/usr/bin/engine"},
	)
	require.NoError(t, err)
	t.Cleanup(probe.Cleanup)

	assert.NotContains(t, probe.Command, fakeBin)
	assert.GreaterOrEqual(
		t,
		strings.Count(probe.Command, nestedProbeEnvPath+" -i"),
		3,
	)
	assert.Contains(
		t,
		probe.Command,
		"PATH="+clcommon.ShellQuoteArg(nestedProbeSystemPath)+"; export PATH",
	)
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
