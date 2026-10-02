package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// GeminiLaunchExecutable is the Gemini CLI entry point frozen on the host
// before a tclaude-layer launch enters a constructed root.
//
// Gemini ships as an npm package whose `gemini` bin is a JavaScript bundle
// started through `#!/usr/bin/env node`. A constructed root mounts only the OS
// surface and the profile's grants, so a Gemini or Node installed under the
// home directory (npm's user prefix, nvm, mise, Homebrew on Linux, …) is
// absent inside it, and so is the PATH directory env would search. Resolving
// both on the host and starting `<node> <bundle>` by absolute path removes the
// shebang's PATH lookup from the sandbox entirely; ReadPaths are the read-only
// grants that make those paths exist there.
type GeminiLaunchExecutable struct {
	// Interpreter is the absolute Node executable, or "" when the entry point
	// is itself a native executable.
	Interpreter string
	// Path is the absolute entry point: the bundle, or the native executable.
	Path string
	// ReadPaths are the host paths the launch must be able to read: the
	// interpreter, the package root, and any of the package's dependencies
	// installed beside it rather than inside it.
	ReadPaths []string
}

// geminiPackageName is the npm package the `gemini` bin belongs to.
const geminiPackageName = "@google/gemini-cli"

// geminiPackageSearchDepth bounds the walk from the bin to its package.json;
// the bin is `bundle/gemini.js`, one level below the package root.
const geminiPackageSearchDepth = 4

// geminiDependencyLimit bounds the hoisted-dependency walk. Gemini ships as a
// bundle; only its optional native modules (node-pty and its per-platform
// builds, keytar) are installed as packages at all.
const geminiDependencyLimit = 32

const geminiLaunchCapability = "gemini_launch_executable"

// ResolveGeminiLaunchExecutable resolves `gemini` from PATH to its real entry
// point, the interpreter its shebang names, and the paths both need.
//
// Callers use it only for a constructed root. A host-inherited root still sees
// the pane's own PATH, profile environment and pre-launch script, and those
// must keep choosing the binary there.
func ResolveGeminiLaunchExecutable() (GeminiLaunchExecutable, error) {
	entry, err := resolveLaunchExecutable("gemini", geminiLaunchCapability)
	if err != nil {
		return GeminiLaunchExecutable{}, err
	}
	return resolveGeminiLaunchFrom(entry.Path, exec.LookPath)
}

// SpliceArgv replaces the `gemini` token of a one-shot argv with the resolved
// entry point. The token is located rather than assumed first: the asker's
// argv may lead with an `env K=V…` prefix carrying the sandbox mode.
func (executable GeminiLaunchExecutable) SpliceArgv(argv []string) ([]string, error) {
	at := slices.Index(argv, "gemini")
	if at < 0 {
		return nil, fmt.Errorf("gemini one-shot argv has no gemini binary")
	}
	entry := []string{executable.Path}
	if executable.Interpreter != "" {
		entry = []string{executable.Interpreter, executable.Path}
	}
	return slices.Concat(argv[:at], entry, argv[at+1:]), nil
}

func geminiLaunchRefusal(format string, args ...any) error {
	return &NestedSandboxCapabilityError{Capability: geminiLaunchCapability, Detail: fmt.Sprintf(format, args...)}
}

func resolveGeminiLaunchFrom(
	entry string,
	lookPath func(string) (string, error),
) (GeminiLaunchExecutable, error) {
	interpreterName, native, err := geminiShebangInterpreter(entry)
	if err != nil {
		return GeminiLaunchExecutable{}, err
	}
	if native {
		if filepath.Base(entry) != "gemini" {
			// A version-manager shim (mise, Volta) is one binary that picks
			// the tool from argv[0]; started by its resolved path it would
			// run as itself.
			return GeminiLaunchExecutable{}, geminiLaunchRefusal(
				"`gemini` on PATH resolves to %q, which looks like a version-manager shim; "+
					"tclaude must start Gemini by its real path inside a constructed root, so put the "+
					"Node install's own bin directory on PATH (e.g. `mise activate` rather than shims)", entry)
		}
		return GeminiLaunchExecutable{Path: entry, ReadPaths: []string{entry}}, nil
	}
	interpreter := interpreterName
	if !filepath.IsAbs(interpreter) {
		interpreter, err = lookPath(interpreterName)
		if err != nil {
			return GeminiLaunchExecutable{}, geminiLaunchRefusal(
				"Gemini entry point %q runs under %q, which is not on PATH: %v", entry, interpreterName, err)
		}
	}
	node, err := inspectExecutableFile(interpreter, interpreterName, geminiLaunchCapability)
	if err != nil {
		return GeminiLaunchExecutable{}, err
	}
	root, ok := geminiPackageRoot(entry)
	if !ok {
		// Mounting the bundle's directory instead could expose an arbitrary
		// directory (~/.local/bin, or the home directory itself).
		return GeminiLaunchExecutable{}, geminiLaunchRefusal(
			"Gemini entry point %q is not inside an installed %s package", entry, geminiPackageName)
	}
	readPaths := append([]string{node.Path, root}, geminiHoistedDependencies(root)...)
	return GeminiLaunchExecutable{Interpreter: node.Path, Path: entry, ReadPaths: readPaths}, nil
}

// geminiShebangInterpreter names the Node interpreter entry's `#!` line runs.
// native reports a file without a `#!` line. `/usr/bin/env node` names node
// for a PATH lookup; an absolute interpreter is returned as is. Anything else
// is refused: another interpreter (an asdf or pnpm shell shim), or a shebang
// that passes the interpreter arguments, which `<node> <bundle>` would drop.
func geminiShebangInterpreter(entry string) (interpreter string, native bool, err error) {
	file, err := os.Open(entry)
	if err != nil {
		return "", false, fmt.Errorf("read Gemini entry point %q: %w", entry, err)
	}
	defer func() { _ = file.Close() }()
	line, _ := bufio.NewReader(io.LimitReader(file, 512)).ReadString('\n')
	if !strings.HasPrefix(line, "#!") {
		return "", true, nil
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
	}
	if len(fields) != 1 || !geminiIsNodeName(filepath.Base(fields[0])) {
		return "", false, geminiLaunchRefusal(
			"Gemini entry point %q starts with %q; tclaude can only start a Node bundle run as "+
				"`#!/usr/bin/env node` or `#!/path/to/node` inside a constructed root "+
				"(a shell shim from asdf or pnpm is not supported there)", entry, strings.TrimSpace(line))
	}
	return fields[0], false, nil
}

func geminiIsNodeName(name string) bool { return name == "node" || name == "nodejs" }

// geminiPackageRoot walks up from the bin to the Gemini package's root.
func geminiPackageRoot(entry string) (string, bool) {
	dir := filepath.Dir(entry)
	for range geminiPackageSearchDepth {
		if name, _, ok := readGeminiPackageManifest(dir); ok && name == geminiPackageName {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// geminiHoistedDependencies returns the dependencies npm placed beside the
// package in the enclosing node_modules (a local or workspace install) rather
// than inside it (a global install), following their own dependencies too:
// node-pty's per-platform build is a dependency of node-pty, not of Gemini.
//
// Every returned path is the symlink-resolved directory, and it must stay
// inside the enclosing node_modules. The names come from installed manifests,
// so a symlink planted there must not turn into a read grant elsewhere.
func geminiHoistedDependencies(root string) []string {
	modules := filepath.Dir(filepath.Dir(root)) // node_modules/@google/gemini-cli
	if filepath.Base(modules) != "node_modules" {
		return nil
	}
	realModules, err := filepath.EvalSymlinks(modules)
	if err != nil {
		return nil
	}
	_, queue, ok := readGeminiPackageManifest(root)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for len(queue) > 0 && len(out) < geminiDependencyLimit {
		dep := queue[0]
		queue = queue[1:]
		if seen[dep] || !geminiValidPackageName(dep) {
			continue
		}
		seen[dep] = true
		if _, err := os.Stat(filepath.Join(root, "node_modules", filepath.FromSlash(dep))); err == nil {
			continue // nested inside the package: covered by its root
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(modules, filepath.FromSlash(dep)))
		if err != nil || !geminiLaunchPathWithin(realModules, resolved) {
			continue
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, resolved)
		if _, more, ok := readGeminiPackageManifest(resolved); ok {
			queue = append(queue, more...)
		}
	}
	return out
}

// geminiValidPackageName accepts `name` and `@scope/name` with no path tricks.
func geminiValidPackageName(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) > 2 || (len(parts) == 2 && !strings.HasPrefix(parts[0], "@")) {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || part == "@" || strings.ContainsAny(part, `\`) {
			return false
		}
	}
	return true
}

func geminiLaunchPathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readGeminiPackageManifest(dir string) (name string, deps []string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", nil, false
	}
	var manifest struct {
		Name                 string            `json:"name"`
		Dependencies         map[string]string `json:"dependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if json.Unmarshal(raw, &manifest) != nil {
		return "", nil, false
	}
	for dep := range manifest.Dependencies {
		deps = append(deps, dep)
	}
	for dep := range manifest.OptionalDependencies {
		if _, dup := manifest.Dependencies[dep]; !dup {
			deps = append(deps, dep)
		}
	}
	sort.Strings(deps)
	return manifest.Name, deps, true
}
