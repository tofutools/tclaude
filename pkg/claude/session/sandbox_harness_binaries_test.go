package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// fakeHarnessInstalls lays out a native Claude behind a PATH symlink, an npm
// Codex whose PATH entry is a Node launcher, and a node executable. OpenCode
// and Copilot are not installed.
func fakeHarnessInstalls(t *testing.T) (root string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	bin := filepath.Join(root, "bin")
	versions := filepath.Join(root, "share", "claude", "versions")
	codexPkg := filepath.Join(root, "lib", "node_modules", "@openai", "codex")
	nodeBin := filepath.Join(root, "node", "bin")
	for _, dir := range []string{bin, versions, filepath.Join(codexPkg, "bin"), nodeBin} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(versions, "2.0.0"), []byte("\x7fELF"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(codexPkg, "bin", "codex.js"),
		[]byte("#!/usr/bin/env node\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nodeBin, "node"), []byte("\x7fELF"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(versions, "2.0.0"), filepath.Join(bin, "claude")))
	require.NoError(t, os.Symlink(filepath.Join(codexPkg, "bin", "codex.js"), filepath.Join(bin, "codex")))
	require.NoError(t, os.Symlink(filepath.Join(nodeBin, "node"), filepath.Join(bin, "node")))

	oldLook, oldCodex := tclaudeLayerHarnessLookPath, tclaudeLayerResolveNativeCodex
	tclaudeLayerHarnessLookPath = func(name string) (string, error) {
		path := filepath.Join(bin, name)
		if _, err := os.Lstat(path); err != nil {
			return "", exec.ErrNotFound
		}
		return path, nil
	}
	tclaudeLayerResolveNativeCodex = func() (harness.NestedSandboxExecutable, error) {
		return harness.NestedSandboxExecutable{}, errors.New("not native")
	}
	t.Cleanup(func() { tclaudeLayerHarnessLookPath, tclaudeLayerResolveNativeCodex = oldLook, oldCodex })
	return root
}

func TestResolveTclaudeLayerHarnessBinaries(t *testing.T) {
	root := fakeHarnessInstalls(t)
	got := ResolveTclaudeLayerHarnessBinaries()
	assert.ElementsMatch(t, []string{
		filepath.Join(root, "share", "claude", "versions", "2.0.0"),
		filepath.Join(root, "lib", "node_modules", "@openai", "codex"),
		filepath.Join(root, "node", "bin", "node"),
	}, got.ReadPaths, "native executables, npm package roots and node are reopened")
	assert.ElementsMatch(t, []string{
		filepath.Join(root, "bin", "claude"),
		filepath.Join(root, "bin", "codex"),
		filepath.Join(root, "bin", "node"),
	}, got.EntryPoints, "symlinked PATH entries must resolve inside the sandbox")
}

func TestBuildTclaudeLayerLaunchSpecExposesInstalledHarnesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("installed-harness exposure is Linux only")
	}
	t.Setenv("HOME", t.TempDir())
	root := fakeHarnessInstalls(t)
	workspace := t.TempDir()

	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:              harness.DefaultName,
		Cwd:                      workspace,
		ExposeInstalledHarnesses: true,
	})
	require.NoError(t, err)
	readable := map[string]bool{}
	for _, grant := range spec.Effective.Filesystem {
		if grant.Access == sandboxpolicy.AccessRead {
			readable[grant.Path] = true
		}
	}
	assert.True(t, readable[filepath.Join(root, "lib", "node_modules", "@openai", "codex")],
		"a Claude launch must be able to run the installed Codex")
	assert.True(t, readable[filepath.Join(root, "node", "bin", "node")])
	assert.Contains(t, spec.Effective.MountAliases, sandboxpolicy.MountAlias{
		Link:   filepath.Join(root, "bin", "codex"),
		Target: filepath.Join(root, "lib", "node_modules", "@openai", "codex", "bin", "codex.js"),
	})

	plain, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: harness.DefaultName,
		Cwd:         workspace,
	})
	require.NoError(t, err)
	assert.Empty(t, plain.Effective.MountAliases, "exposure is opt-in per launch path")
}

// The standalone Codex installer links ~/.local/bin/codex at
// ~/.codex/packages/standalone/current/bin/codex, where `current` is itself a
// symlink to a release. A sandbox that shows the host's own PATH link needs
// `current` too, or the link dangles.
func TestTclaudeLayerEntryPointAliasesFollowLinkChain(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	release := filepath.Join(root, ".codex", "packages", "standalone", "releases", "0.155.1")
	current := filepath.Join(root, ".codex", "packages", "standalone", "current")
	bin := filepath.Join(root, ".local", "bin")
	require.NoError(t, os.MkdirAll(filepath.Join(release, "bin"), 0o755))
	require.NoError(t, os.MkdirAll(bin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(release, "bin", "codex"), []byte("\x7fELF"), 0o755))
	require.NoError(t, os.Symlink("releases/0.155.1", current))
	require.NoError(t, os.Symlink(filepath.Join(current, "bin", "codex"), filepath.Join(bin, "codex")))

	aliases := tclaudeLayerEntryPointAliases([]string{filepath.Join(bin, "codex")})
	assert.Contains(t, aliases, sandboxpolicy.MountAlias{
		Link: filepath.Join(bin, "codex"), Target: filepath.Join(release, "bin", "codex"),
	})
	assert.Contains(t, aliases, sandboxpolicy.MountAlias{Link: current, Target: release},
		"the intermediate `current` link must be recreated too")
}
