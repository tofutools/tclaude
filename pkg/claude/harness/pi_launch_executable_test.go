package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// piManagedFixture lays out what pi 1.5's managed installer leaves under
// home: the `#!/bin/sh` launcher at ~/.pi/agent/bin/pi, the version file, and
// the release's npm install with its `.bin/pi` link to the CLI. It returns
// the launcher, the install root and the CLI.
func piManagedFixture(t *testing.T, home, version string) (launcher, install, cli string) {
	t.Helper()
	agent := filepath.Join(home, ".pi", "agent")
	launcher = filepath.Join(agent, "bin", "pi")
	require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o755))
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$root/releases/$v/node_modules/.bin/pi\" \"$@\"\n"), 0o755))
	install = filepath.Join(agent, "install")
	require.NoError(t, os.MkdirAll(install, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(install, "current-version"), []byte(version+"\n"), 0o644))
	modules := filepath.Join(install, "releases", version, "node_modules")
	pkg := filepath.Join(modules, "@earendil-works", "pi-coding-agent")
	require.NoError(t, os.MkdirAll(filepath.Join(pkg, "dist", "bundle"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{
		"name": "@earendil-works/pi-coding-agent",
		"bin": {"pi": "dist/bundle/cli.js"},
		"dependencies": {"undici": "7"}
	}`), 0o644))
	cli = filepath.Join(pkg, "dist", "bundle", "cli.js")
	require.NoError(t, os.WriteFile(cli, []byte("#!/usr/bin/env node\nimport './x.js';\n"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(modules, "undici"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(modules, ".bin"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("..", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js"),
		filepath.Join(modules, ".bin", "pi")))
	return launcher, install, cli
}

// piInstallerNodeFixture is the installer's Node: pi-node/current, a link to
// the versioned directory, under the data home.
func piInstallerNodeFixture(t *testing.T, dataHome string) string {
	t.Helper()
	node := geminiFakeNode(t, filepath.Join(dataHome, "pi-node", "v22.23.4", "bin"))
	require.NoError(t, os.Symlink("v22.23.4", filepath.Join(dataHome, "pi-node", "current")))
	return node
}

func noPathNode(string) (string, error) {
	return "", errors.New("node is not on PATH")
}

func TestResolvePiManagedInstallStartsItsCLIWithTheInstallersNode(t *testing.T) {
	home := geminiRealTempDir(t)
	launcher, install, cli := piManagedFixture(t, home, "1.5.0")
	dataHome := filepath.Join(home, ".local", "share")
	node := piInstallerNodeFixture(t, dataHome)

	got, err := resolvePiLaunchFrom(launcher, noPathNode, dataHome)
	require.NoError(t, err)
	assert.Equal(t, node, got.Interpreter, "the installer's Node, symlink-resolved, though node is not on PATH")
	assert.Equal(t, cli, got.Path, "the CLI the launcher would exec, not the /bin/sh launcher")
	modules := filepath.Join(install, "releases", "1.5.0", "node_modules")
	assert.Equal(t, []string{
		node,
		filepath.Join(modules, "@earendil-works", "pi-coding-agent"),
		filepath.Join(modules, "undici"),
		filepath.Join(install, "current-version"),
	}, got.ReadPaths)
	assert.Equal(t, install, PiManagedInstallRoot(got.Path))
}

func TestResolvePiManagedInstallFallsBackToPathNode(t *testing.T) {
	home := geminiRealTempDir(t)
	launcher, _, _ := piManagedFixture(t, home, "1.5.0")
	node := geminiFakeNode(t, filepath.Join(home, "nvm", "bin"))

	got, err := resolvePiLaunchFrom(launcher, func(name string) (string, error) {
		assert.Equal(t, "node", name)
		return node, nil
	}, filepath.Join(home, "no-data-home"))
	require.NoError(t, err)
	assert.Equal(t, node, got.Interpreter)
}

func TestResolvePiManagedInstallRefusesAVersionThatIsNotOne(t *testing.T) {
	for _, version := range []string{"", "..", "../../etc", "1.5.0/../../x", ".hidden", "1.5.0 extra", strings.Repeat("9", 65)} {
		t.Run(version, func(t *testing.T) {
			home := geminiRealTempDir(t)
			launcher, install, _ := piManagedFixture(t, home, "1.5.0")
			require.NoError(t, os.WriteFile(filepath.Join(install, "current-version"), []byte(version), 0o644))
			_, err := resolvePiLaunchFrom(launcher, noPathNode, "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "is not a version")
		})
	}
}

func TestResolvePiManagedInstallRefusals(t *testing.T) {
	t.Run("release missing", func(t *testing.T) {
		home := geminiRealTempDir(t)
		launcher, install, _ := piManagedFixture(t, home, "1.5.0")
		require.NoError(t, os.WriteFile(filepath.Join(install, "current-version"), []byte("1.6.0\n"), 0o644))
		_, err := resolvePiLaunchFrom(launcher, noPathNode, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no CLI for release 1.6.0")
	})
	t.Run("CLI link leaves the release", func(t *testing.T) {
		home := geminiRealTempDir(t)
		launcher, install, _ := piManagedFixture(t, home, "1.5.0")
		elsewhere := filepath.Join(home, "elsewhere.js")
		require.NoError(t, os.WriteFile(elsewhere, []byte("#!/usr/bin/env node\n"), 0o755))
		link := filepath.Join(install, "releases", "1.5.0", "node_modules", ".bin", "pi")
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(elsewhere, link))
		_, err := resolvePiLaunchFrom(launcher, noPathNode, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolves outside its release")
	})
	t.Run("a shell shim outside the managed layout", func(t *testing.T) {
		home := geminiRealTempDir(t)
		shim := filepath.Join(home, ".asdf", "shims", "pi")
		require.NoError(t, os.MkdirAll(filepath.Dir(shim), 0o755))
		require.NoError(t, os.WriteFile(shim, []byte("#!/usr/bin/env bash\nexec asdf exec pi \"$@\"\n"), 0o755))
		_, err := resolvePiLaunchFrom(shim, noPathNode, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shell shim", "only pi's own launcher is recognised")
	})
}

func TestPiManagedInstallRootIsEmptyForAnNpmInstall(t *testing.T) {
	home := geminiRealTempDir(t)
	cli := filepath.Join(home, "lib", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js")
	require.NoError(t, os.MkdirAll(filepath.Dir(cli), 0o755))
	require.NoError(t, os.WriteFile(cli, []byte("#!/usr/bin/env node\n"), 0o755))
	assert.Empty(t, PiManagedInstallRoot(cli))

	// An npm install under some releases/ directory beside a version file is
	// not the installer's layout either.
	deploy := filepath.Join(home, "deploy")
	local := filepath.Join(deploy, "releases", "2024", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "cli.js")
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	require.NoError(t, os.WriteFile(local, []byte("#!/usr/bin/env node\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deploy, "current-version"), []byte("2024\n"), 0o644))
	assert.Empty(t, PiManagedInstallRoot(local))
}

func TestPiBuildCommandExportsTheManagedInstallRoot(t *testing.T) {
	home := geminiRealTempDir(t)
	_, install, cli := piManagedFixture(t, home, "1.5.0")
	command := piSpawner{}.BuildCommand(SpawnSpec{ExecutablePath: cli, ExecutableInterpreter: "/opt/node"})
	assert.Contains(t, command, "export "+PiManagedInstallEnvVar+"="+install+"; ")

	npm := piSpawner{}.BuildCommand(SpawnSpec{ExecutablePath: "/usr/lib/node_modules/pi/cli.js"})
	assert.NotContains(t, npm, PiManagedInstallEnvVar)
}
