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
type GeminiLaunchExecutable = NodeLaunchExecutable

// NodeLaunchExecutable is a Node CLI's entry point frozen on the host: the
// shape Gemini CLI and pi share (see GeminiLaunchExecutable).
type NodeLaunchExecutable struct {
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

// nodeCLIPackage names one npm-installed Node CLI the resolver below can
// start by absolute path.
type nodeCLIPackage struct {
	// bin is the PATH name, pkg the npm package it belongs to, display the
	// harness name in refusals.
	bin, pkg, display string
	capability        string
	// searchDepth bounds the walk from the bin to its package.json.
	searchDepth int
	// depLimit bounds the hoisted-dependency walk.
	depLimit int
}

// geminiCLIPackage: the bin is `bundle/gemini.js`, one level below the package
// root. Gemini ships as a bundle; only its optional native modules (node-pty
// and its per-platform builds, keytar) are installed as packages at all.
var geminiCLIPackage = nodeCLIPackage{
	bin: "gemini", pkg: "@google/gemini-cli", display: "Gemini",
	capability: geminiLaunchCapability, searchDepth: 4, depLimit: 32,
}

const geminiLaunchCapability = "gemini_launch_executable"

// ResolveGeminiLaunchExecutable resolves `gemini` from PATH to its real entry
// point, the interpreter its shebang names, and the paths both need.
//
// Callers use it only for a constructed root. A host-inherited root still sees
// the pane's own PATH, profile environment and pre-launch script, and those
// must keep choosing the binary there.
func ResolveGeminiLaunchExecutable() (GeminiLaunchExecutable, error) {
	return geminiCLIPackage.resolve()
}

func (p nodeCLIPackage) resolve() (NodeLaunchExecutable, error) {
	entry, err := resolveLaunchExecutable(p.bin, p.capability)
	if err != nil {
		return NodeLaunchExecutable{}, err
	}
	return p.resolveFrom(entry.Path, exec.LookPath)
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

func (p nodeCLIPackage) refusal(format string, args ...any) error {
	return &NestedSandboxCapabilityError{Capability: p.capability, Detail: fmt.Sprintf(format, args...)}
}

func resolveGeminiLaunchFrom(
	entry string,
	lookPath func(string) (string, error),
) (GeminiLaunchExecutable, error) {
	return geminiCLIPackage.resolveFrom(entry, lookPath)
}

func (p nodeCLIPackage) resolveFrom(
	entry string,
	lookPath func(string) (string, error),
) (NodeLaunchExecutable, error) {
	interpreterName, native, err := p.shebangInterpreter(entry)
	if err != nil {
		return NodeLaunchExecutable{}, err
	}
	if native {
		if filepath.Base(entry) != p.bin {
			// A version-manager shim (mise, Volta) is one binary that picks
			// the tool from argv[0]; started by its resolved path it would
			// run as itself.
			return NodeLaunchExecutable{}, p.refusal(
				"`%s` on PATH resolves to %q, which looks like a version-manager shim; "+
					"tclaude must start %s by its real path inside a constructed root, so put the "+
					"Node install's own bin directory on PATH (e.g. `mise activate` rather than shims)", p.bin, entry, p.display)
		}
		return NodeLaunchExecutable{Path: entry, ReadPaths: []string{entry}}, nil
	}
	interpreter := interpreterName
	if !filepath.IsAbs(interpreter) {
		interpreter, err = lookPath(interpreterName)
		if err != nil {
			return NodeLaunchExecutable{}, p.refusal(
				"%s entry point %q runs under %q, which is not on PATH: %v", p.display, entry, interpreterName, err)
		}
	}
	node, err := inspectExecutableFile(interpreter, interpreterName, p.capability)
	if err != nil {
		return NodeLaunchExecutable{}, err
	}
	root, ok := p.packageRoot(entry)
	if !ok {
		// Mounting the bundle's directory instead could expose an arbitrary
		// directory (~/.local/bin, or the home directory itself).
		return NodeLaunchExecutable{}, p.refusal(
			"%s entry point %q is not inside an installed %s package", p.display, entry, p.pkg)
	}
	readPaths := append([]string{node.Path, root}, p.hoistedDependencies(root)...)
	return NodeLaunchExecutable{Interpreter: node.Path, Path: entry, ReadPaths: readPaths}, nil
}

// shebangInterpreter names the Node interpreter entry's `#!` line runs.
// native reports a file without a `#!` line. `/usr/bin/env node` names node
// for a PATH lookup; an absolute interpreter is returned as is. Anything else
// is refused: another interpreter (an asdf or pnpm shell shim), or a shebang
// that passes the interpreter arguments, which `<node> <bundle>` would drop.
func (p nodeCLIPackage) shebangInterpreter(entry string) (interpreter string, native bool, err error) {
	file, err := os.Open(entry)
	if err != nil {
		return "", false, fmt.Errorf("read %s entry point %q: %w", p.display, entry, err)
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
		return "", false, p.refusal(
			"%s entry point %q starts with %q; tclaude can only start a Node bundle run as "+
				"`#!/usr/bin/env node` or `#!/path/to/node` inside a constructed root "+
				"(a shell shim from asdf or pnpm is not supported there)", p.display, entry, strings.TrimSpace(line))
	}
	return fields[0], false, nil
}

func geminiIsNodeName(name string) bool { return name == "node" || name == "nodejs" }

// packageRoot walks up from the bin to the package's root.
func (p nodeCLIPackage) packageRoot(entry string) (string, bool) {
	dir := filepath.Dir(entry)
	for range p.searchDepth {
		if name, _, ok := readGeminiPackageManifest(dir); ok && name == p.pkg {
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

// hoistedDependencies returns the dependencies npm placed beside the
// package in the enclosing node_modules (a local or workspace install) rather
// than inside it (a global install), following their own dependencies too:
// node-pty's per-platform build is a dependency of node-pty, not of Gemini.
//
// Every returned path is the symlink-resolved directory, and it must stay
// inside the enclosing node_modules. The names come from installed manifests,
// so a symlink planted there must not turn into a read grant elsewhere.
func (p nodeCLIPackage) hoistedDependencies(root string) []string {
	modules := filepath.Dir(filepath.Dir(root)) // node_modules/@scope/name
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
	for len(queue) > 0 && len(out) < p.depLimit {
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
