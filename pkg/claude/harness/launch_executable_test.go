package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestCodexNPMNativeBackendDoesNotSearchAboveLauncherPackage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Codex native runtime resolution is Linux-only")
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

func TestResolveClaudeLaunchExecutableDoesNotExecuteCandidate(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "executed")
	executable := filepath.Join(root, "claude")
	require.NoError(t, os.WriteFile(executable, []byte(
		"#!/bin/sh\ntouch "+clcommon.ShellQuoteArg(sentinel)+"\n"), 0o700))
	t.Setenv("PATH", root)

	resolved, err := ResolveClaudeLaunchExecutable()
	require.NoError(t, err)
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	require.NoError(t, err)
	assert.Equal(t, canonicalExecutable, resolved.Path)
	assert.NoFileExists(t, sentinel,
		"ordinary tclaude-layer resolution must not run code outside the sandbox")
}

func TestResolveCodexNativeLaunchExecutableDoesNotProbeVersion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Codex native runtime resolution is Linux-only")
	}
	packageName, targetTriple, err := codexLinuxNativeTarget()
	require.NoError(t, err)
	root := testutil.CanonicalTempDir(t)
	launcher := filepath.Join(root, "node_modules", "@openai", "codex", "bin", "codex.js")
	require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o700))
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\nexit 99\n"), 0o700))
	packageParts := strings.Split(packageName, "/")
	native := filepath.Join(
		root, "node_modules", packageParts[0], packageParts[1], "vendor",
		targetTriple, "bin", "codex")
	sentinel := filepath.Join(root, "native-executed")
	require.NoError(t, os.MkdirAll(filepath.Dir(native), 0o700))
	require.NoError(t, os.WriteFile(native, []byte(
		"#!/bin/sh\ntouch "+clcommon.ShellQuoteArg(sentinel)+"\n"), 0o700))
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
		NestedSandboxExecutable{Path: launcher},
		"codex_launch_executable",
	)
	require.NoError(t, err)
	assert.Equal(t, native, resolved.Path)
	assert.Equal(t, nativeRoot, resolved.RuntimeRoot)
	assert.NoFileExists(t, sentinel,
		"ordinary tclaude-layer resolution must not run the native backend")
}
