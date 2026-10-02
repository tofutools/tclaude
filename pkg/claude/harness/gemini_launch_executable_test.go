package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// geminiLaunchFixture lays out an npm install of the Gemini package under
// modules (a node_modules directory) and returns the bundle path.
func geminiLaunchFixture(t *testing.T, modules, shebang string, nested bool) string {
	t.Helper()
	root := filepath.Join(modules, "@google", "gemini-cli")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bundle"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{
		"name": "@google/gemini-cli",
		"bin": {"gemini": "bundle/gemini.js"},
		"optionalDependencies": {"@lydell/node-pty": "1", "@github/keytar": "1", "node-pty": "1"}
	}`), 0o644))
	entry := filepath.Join(root, "bundle", "gemini.js")
	require.NoError(t, os.WriteFile(entry, []byte(shebang+"\nimport './x.js';\n"), 0o755))
	ptyParent := modules
	if nested {
		ptyParent = filepath.Join(root, "node_modules")
	}
	require.NoError(t, os.MkdirAll(filepath.Join(ptyParent, "@lydell", "node-pty"), 0o755))
	return entry
}

func geminiFakeNode(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	node := filepath.Join(dir, "node")
	require.NoError(t, os.WriteFile(node, []byte("#!/bin/sh\n"), 0o755))
	return node
}

func TestResolveGeminiLaunchHoistedInstall(t *testing.T) {
	home := t.TempDir()
	modules := filepath.Join(home, "proj", "node_modules")
	entry := geminiLaunchFixture(t, modules, "#!/usr/bin/env node", false)
	node := geminiFakeNode(t, filepath.Join(home, ".local", "share", "mise", "installs", "node", "26", "bin"))
	// PATH reaches node through a version-alias symlink, which a constructed
	// root would not carry; the resolved target is what must be mounted.
	latest := filepath.Join(home, ".local", "share", "mise", "installs", "node", "latest")
	require.NoError(t, os.Symlink(filepath.Dir(filepath.Dir(node)), latest))

	got, err := resolveGeminiLaunchFrom(entry, func(name string) (string, error) {
		assert.Equal(t, "node", name)
		return filepath.Join(latest, "bin", "node"), nil
	})
	require.NoError(t, err)
	assert.Equal(t, node, got.Interpreter, "the interpreter is the symlink-resolved binary")
	assert.Equal(t, entry, got.Path)
	assert.Equal(t, []string{
		node,
		filepath.Join(modules, "@google", "gemini-cli"),
		filepath.Join(modules, "@lydell", "node-pty"),
	}, got.ReadPaths, "a hoisted optional dependency is mounted; an absent one is not")
}

func TestResolveGeminiLaunchGlobalInstallWithAbsoluteShebang(t *testing.T) {
	home := t.TempDir()
	node := geminiFakeNode(t, filepath.Join(home, ".nvm", "versions", "node", "v24", "bin"))
	modules := filepath.Join(home, ".nvm", "versions", "node", "v24", "lib", "node_modules")
	entry := geminiLaunchFixture(t, modules, "#!"+node, true)

	got, err := resolveGeminiLaunchFrom(entry, func(string) (string, error) {
		return "", errors.New("PATH must not be consulted for an absolute shebang")
	})
	require.NoError(t, err)
	assert.Equal(t, node, got.Interpreter)
	assert.Equal(t, []string{node, filepath.Join(modules, "@google", "gemini-cli")}, got.ReadPaths,
		"dependencies nested in the package are covered by its root")
}

func TestResolveGeminiLaunchFollowsHoistedDependenciesWithinNodeModules(t *testing.T) {
	home := t.TempDir()
	modules := filepath.Join(home, "proj", "node_modules")
	entry := geminiLaunchFixture(t, modules, "#!/usr/bin/env node", false)
	// node-pty's per-platform build is ITS dependency, not Gemini's.
	pty := filepath.Join(modules, "@lydell", "node-pty")
	require.NoError(t, os.WriteFile(filepath.Join(pty, "package.json"),
		[]byte(`{"name":"@lydell/node-pty","optionalDependencies":{"@lydell/node-pty-linux-x64":"1"}}`), 0o644))
	platform := filepath.Join(modules, "@lydell", "node-pty-linux-x64")
	require.NoError(t, os.MkdirAll(platform, 0o755))
	// A dependency symlinked out of node_modules must not become a grant.
	secret := filepath.Join(home, ".ssh")
	require.NoError(t, os.MkdirAll(secret, 0o700))
	require.NoError(t, os.Symlink(secret, filepath.Join(modules, "node-pty")))
	node := geminiFakeNode(t, filepath.Join(home, "bin"))

	got, err := resolveGeminiLaunchFrom(entry, func(string) (string, error) { return node, nil })
	require.NoError(t, err)
	assert.Equal(t, []string{node, filepath.Join(modules, "@google", "gemini-cli"), pty, platform}, got.ReadPaths)
	assert.NotContains(t, got.ReadPaths, secret)
}

func TestResolveGeminiLaunchRefusals(t *testing.T) {
	dir := t.TempDir()
	native := filepath.Join(dir, "gemini")
	require.NoError(t, os.WriteFile(native, []byte("\x7fELF"), 0o755))
	got, err := resolveGeminiLaunchFrom(native, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Interpreter, "a native build runs as itself")
	assert.Equal(t, []string{native}, got.ReadPaths)

	// mise and Volta shims are one binary choosing the tool from argv[0].
	shim := filepath.Join(dir, "mise")
	require.NoError(t, os.WriteFile(shim, []byte("\x7fELF"), 0o755))
	_, err = resolveGeminiLaunchFrom(shim, nil)
	require.ErrorContains(t, err, "version-manager shim")

	for name, shebang := range map[string]string{
		"asdf shell shim":          "#!/usr/bin/env bash",
		"interpreter arguments":    "#!/usr/bin/env -S node --no-warnings",
		"env options":              "#!/usr/bin/env -u FOO node",
		"node-prefixed other tool": "#!/usr/bin/env nodemon",
	} {
		path := filepath.Join(dir, "gemini-"+filepath.Base(t.Name())+"-"+name)
		require.NoError(t, os.WriteFile(path, []byte(shebang+"\n"), 0o755))
		_, err = resolveGeminiLaunchFrom(path, nil)
		require.Errorf(t, err, "%s must be refused", name)
	}

	loose := filepath.Join(dir, "loose", "gemini.js")
	require.NoError(t, os.MkdirAll(filepath.Dir(loose), 0o755))
	require.NoError(t, os.WriteFile(loose, []byte("#!/usr/bin/env node\n"), 0o755))
	node := geminiFakeNode(t, filepath.Join(dir, "bin"))
	_, err = resolveGeminiLaunchFrom(loose, func(string) (string, error) { return node, nil })
	require.ErrorContains(t, err, "not inside an installed", "an unrecognized layout mounts nothing")

	entry := geminiLaunchFixture(t, filepath.Join(dir, "node_modules"), "#!/usr/bin/env node", false)
	_, err = resolveGeminiLaunchFrom(entry, func(string) (string, error) {
		return "", errors.New("not found")
	})
	require.ErrorContains(t, err, "not on PATH")
}

func TestGeminiLaunchSpliceArgv(t *testing.T) {
	node := GeminiLaunchExecutable{Interpreter: "/n/node", Path: "/p/gemini.js"}
	got, err := node.SpliceArgv([]string{"env", "GEMINI_SANDBOX=false", "SANDBOX=", "gemini", "--prompt=gemini"})
	require.NoError(t, err)
	assert.Equal(t, []string{"env", "GEMINI_SANDBOX=false", "SANDBOX=", "/n/node", "/p/gemini.js", "--prompt=gemini"}, got)

	got, err = GeminiLaunchExecutable{Path: "/p/gemini"}.SpliceArgv([]string{"gemini", "-m", "x"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/p/gemini", "-m", "x"}, got)

	_, err = node.SpliceArgv([]string{"env", "X=1"})
	require.Error(t, err)
}
