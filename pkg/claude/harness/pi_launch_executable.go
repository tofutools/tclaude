package harness

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// piCLIPackage is pi's npm package. The `pi` bin is dist/bundle/cli.js, two
// levels below the package root, started through `#!/usr/bin/env node`. Unlike
// Gemini's, pi's bundle loads its runtime dependencies (pi-ai, the TUI, jiti
// for extensions, undici, …) as installed packages, so a local install hoists
// several dozen beside it; a global install keeps them inside the package.
var piCLIPackage = nodeCLIPackage{
	bin: "pi", pkg: "@earendil-works/pi-coding-agent", display: "pi",
	capability: "pi_launch_executable", searchDepth: 4, depLimit: 256,
}

// PiManagedInstallEnvVar is what pi's managed launcher exports to the CLI it
// starts: the install root, which tells pi it was installed by its own
// installer rather than by npm.
const PiManagedInstallEnvVar = "PI_MANAGED_INSTALL_ROOT"

// ResolvePiLaunchExecutable resolves `pi` from PATH to its entry point, its
// Node interpreter and the paths both need, for a launch inside a
// constructed root (see GeminiLaunchExecutable for why).
//
// pi's own installer puts a `#!/bin/sh` launcher on PATH rather than the npm
// bin (see piManagedLauncher); the CLI it would start is resolved instead.
func ResolvePiLaunchExecutable() (NodeLaunchExecutable, error) {
	entry, err := resolveLaunchExecutable(piCLIPackage.bin, piCLIPackage.capability)
	if err != nil {
		return NodeLaunchExecutable{}, err
	}
	return resolvePiLaunchFrom(entry.Path, exec.LookPath, piDataHome())
}

func resolvePiLaunchFrom(
	entry string,
	lookPath func(string) (string, error),
	dataHome string,
) (NodeLaunchExecutable, error) {
	cli, managed, err := piManagedLauncher(entry)
	if err != nil {
		return NodeLaunchExecutable{}, err
	}
	if !managed {
		return piCLIPackage.resolveFrom(entry, lookPath)
	}
	executable, err := piCLIPackage.resolveFrom(cli, piInstallerNode(dataHome, lookPath))
	if err != nil {
		return NodeLaunchExecutable{}, err
	}
	// PiManagedInstallEnvVar names the install root to pi; its version file is
	// the one thing there beyond the release that pi may read.
	if install := PiManagedInstallRoot(executable.Path); install != "" {
		executable.ReadPaths = append(executable.ReadPaths, filepath.Join(install, "current-version"))
	}
	return executable, nil
}

// piManagedVersion is the shape a release directory name must have before it
// is joined into a path: the version file is read from disk, so it must not
// be able to name a directory outside releases/.
var piManagedVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// piManagedLauncher recognises the launcher pi's managed installer puts on
// PATH (pi 1.5): `<agent>/bin/pi`, a `#!/bin/sh` script beside
// `<agent>/install/current-version`, which execs
// `<agent>/install/releases/<version>/node_modules/.bin/pi` with the
// installer's Node on PATH. It is recognised by that layout, never by running
// or parsing the script, and cli is the CLI the launcher would exec, symlinks
// resolved. managed is false for any other entry point, which keeps its
// ordinary resolution.
func piManagedLauncher(entry string) (cli string, managed bool, err error) {
	if filepath.Base(entry) != "pi" || filepath.Base(filepath.Dir(entry)) != "bin" {
		return "", false, nil
	}
	install := filepath.Join(filepath.Dir(filepath.Dir(entry)), "install")
	raw, err := os.ReadFile(filepath.Join(install, "current-version"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, piCLIPackage.refusal("read pi's managed install version in %q: %v", install, err)
	}
	version := strings.TrimSpace(string(raw))
	if !piManagedVersion.MatchString(version) || strings.Contains(version, "..") {
		return "", false, piCLIPackage.refusal(
			"pi's managed install in %q names release %q, which is not a version", install, version)
	}
	release := filepath.Join(install, "releases", version)
	bin := filepath.Join(release, "node_modules", ".bin", "pi")
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return "", false, piCLIPackage.refusal(
			"pi's managed install in %q has no CLI for release %s: %v", install, version, err)
	}
	// The release is a symlink-resolved tree like any other; the CLI must
	// stay inside it, so a planted link cannot turn into a grant elsewhere.
	realRelease, err := filepath.EvalSymlinks(release)
	if err != nil || !geminiLaunchPathWithin(realRelease, resolved) {
		return "", false, piCLIPackage.refusal(
			"pi's managed CLI %q resolves outside its release %q", bin, release)
	}
	return resolved, true, nil
}

// PiManagedInstallRoot is the managed install a resolved pi CLI belongs to
// (`<agent>/install`, what the launcher exports as PiManagedInstallEnvVar),
// or "" when cli is not under one.
func PiManagedInstallRoot(cli string) string {
	for dir := filepath.Dir(cli); ; dir = filepath.Dir(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		if filepath.Base(parent) == "releases" {
			install := filepath.Dir(parent)
			if info, err := os.Stat(filepath.Join(install, "current-version")); err == nil && info.Mode().IsRegular() {
				return install
			}
		}
	}
}

// piInstallerNode prefers the Node pi's installer ships, as its launcher
// does by putting it first on PATH: `node` is often not on the operator's
// PATH at all. Without it, PATH's node is used.
func piInstallerNode(dataHome string, lookPath func(string) (string, error)) func(string) (string, error) {
	return func(name string) (string, error) {
		if dataHome != "" && geminiIsNodeName(name) {
			node := filepath.Join(dataHome, "pi-node", "current", "bin", "node")
			if info, err := os.Stat(node); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				return node, nil
			}
		}
		return lookPath(name)
	}
}

// piDataHome is where pi's installer keeps its Node:
// ${XDG_DATA_HOME:-~/.local/share}.
func piDataHome() string {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share")
}
