package session

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestBuildExecutionBoundaryRecordsInjectedCLIPathAndIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/host/bin:/usr/bin")
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	launcher := filepath.Join(home, "bwrap")
	tclaude := filepath.Join(home, "tclaude-real")
	harnessBinary := filepath.Join(home, "claude-real")
	for _, path := range []string{launcher, tclaude, harnessBinary} {
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o700))
	}
	canonicalLauncher, err := filepath.EvalSymlinks(launcher)
	require.NoError(t, err)
	canonicalHarnessBinary, err := filepath.EvalSymlinks(harnessBinary)
	require.NoError(t, err)
	previousCLI := tclaudeLayerTclaudeCLIPath
	tclaudeLayerTclaudeCLIPath = func() string { return tclaude }
	t.Cleanup(func() { tclaudeLayerTclaudeCLIPath = previousCLI })

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
	snapshot.Effective.PreLaunch = []sandboxpolicy.PreLaunchBlock{{
		Name: "tools", Script: `export PATH="$HOME/tools:$PATH"`, Exports: []string{"PATH"},
	}}
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName, Cwd: cwd, Snapshot: &snapshot,
		HarnessReadPaths: []string{harnessBinary},
	})
	require.NoError(t, err)

	boundary, err := BuildExecutionBoundary(ExecutionBoundaryInput{
		SandboxImplementation: "tclaude-layer", HarnessName: harness.DefaultName,
		HarnessLookupName: "claude", HarnessExecutable: harnessBinary,
		HarnessRuntimeRoots: []string{harnessBinary}, LauncherBinary: launcher,
		Environment: map[string]string{"PATH": "/profile/bin:/usr/bin"}, LayerSpec: &spec,
	})
	require.NoError(t, err)
	if runtime.GOOS == "linux" {
		assert.Equal(t, "constructed", boundary.RootMode)
		require.NotNil(t, boundary.Tclaude)
		assert.Equal(t, tclaude, boundary.Tclaude.HostPath)
		assert.Equal(t, tclaudeLayerConstructedRootTclaudePath, boundary.Tclaude.SandboxPath)
		assert.Equal(t, "/.tclaude/bin:/profile/bin:/usr/bin", boundary.PATH.BeforePreLaunch)
	} else {
		assert.Equal(t, "host-inherited (Seatbelt policy; no mount namespace)", boundary.RootMode)
		assert.Nil(t, boundary.Tclaude)
		assert.Equal(t, "/profile/bin:/usr/bin", boundary.PATH.BeforePreLaunch)
	}
	assert.False(t, boundary.PATH.FinalValueKnown)
	assert.Equal(t, []string{"tools"}, boundary.PATH.PreLaunchDeclaresPATH)
	assert.Equal(t, canonicalHarnessBinary, boundary.Harness.HostPath)
	assert.Equal(t, canonicalLauncher, boundary.Launcher.HostPath)
	if runtime.GOOS == "linux" {
		assert.Contains(t, boundary.AutomaticEntries, ExecutionNamespaceEntry{
			Kind: "bind", Source: tclaude, Target: tclaudeLayerConstructedRootTclaudePath,
			Access: "read-only", Origin: "tclaude coordination CLI",
		})
		assert.Contains(t, boundary.AutomaticEntries, ExecutionNamespaceEntry{
			Kind:   "bind",
			Source: filepath.Join(home, ".tclaude", "data", tclaudeLayerConstructedRootBashEnvState),
			Target: tclaudeLayerConstructedRootBashEnvPath,
			Access: "read-only", Origin: "constructed-root Bash PATH repair",
		})
		assert.Equal(t, os.Getuid(), boundary.Identity.Host.UID)
		assert.Equal(t, os.Getgid(), boundary.Identity.Host.GID)
	}
}

func TestBuildExecutionBoundaryResolvesHarnessFromInjectedPATH(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	executable := filepath.Join(bin, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("fixture"), 0o700))
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	require.NoError(t, err)

	boundary, err := BuildExecutionBoundary(ExecutionBoundaryInput{
		SandboxImplementation: "harness-builtin", HarnessName: "codex", HarnessLookupName: "codex",
		Cwd: root, Environment: map[string]string{"PATH": bin + ":/usr/bin"},
	})
	require.NoError(t, err)
	assert.Equal(t, canonicalExecutable, boundary.Harness.HostPath)
	assert.Equal(t, canonicalExecutable, boundary.Harness.SandboxPath)
	assert.Equal(t, "resolved from launch PATH before exec", boundary.Harness.Resolution)
	assert.Equal(t, bin+":/usr/bin", boundary.PATH.BeforePreLaunch)
}

func TestBuildExecutionBoundaryRecordsDistinctHostAndSandboxExecutable(t *testing.T) {
	hostExecutable := filepath.Join(t.TempDir(), "staged-codex")
	require.NoError(t, os.WriteFile(hostExecutable, []byte("fixture"), 0o700))
	canonicalHostExecutable, err := filepath.EvalSymlinks(hostExecutable)
	require.NoError(t, err)

	boundary, err := BuildExecutionBoundary(ExecutionBoundaryInput{
		HarnessName: "codex", HarnessLookupName: "codex",
		HarnessExecutable: hostExecutable, HarnessSandboxPath: "/tmp/.tclaude-stacked/engine",
	})
	require.NoError(t, err)
	assert.Equal(t, canonicalHostExecutable, boundary.Harness.HostPath)
	assert.Equal(t, "/tmp/.tclaude-stacked/engine", boundary.Harness.SandboxPath)
}

func TestBuildExecutionBoundaryRecordsStagedRuntimeClosure(t *testing.T) {
	hostExecutable := filepath.Join(t.TempDir(), "staged-codex")
	runtimeFile := filepath.Join(t.TempDir(), "runtime-lib")
	require.NoError(t, os.WriteFile(hostExecutable, []byte("engine"), 0o700))
	require.NoError(t, os.WriteFile(runtimeFile, []byte("library"), 0o600))

	boundary, err := BuildExecutionBoundary(ExecutionBoundaryInput{
		HarnessName: "codex", HarnessLookupName: "codex",
		HarnessExecutable: hostExecutable, HarnessSandboxPath: "/tmp/.tclaude-stacked-codex/bin/codex",
		HarnessRuntimeRoots: []string{"/tmp/.tclaude-stacked-codex"},
		HarnessRuntimeBindings: []StackedSandboxRuntimeBinding{{
			HostPath: runtimeFile, SandboxPath: "/tmp/.tclaude-stacked-codex/lib/runtime-lib",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/tmp/.tclaude-stacked-codex"}, boundary.Harness.RuntimeRoots)
	assert.Contains(t, boundary.AutomaticEntries, ExecutionNamespaceEntry{
		Kind: "bind", Source: runtimeFile, Target: "/tmp/.tclaude-stacked-codex/lib/runtime-lib",
		Access: "read-only", Origin: "staged nested harness runtime closure",
	})
}
