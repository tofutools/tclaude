package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// ResolveGeminiLaunchExecutable resolves `gemini` from PATH to its real entry
// point, the interpreter its shebang names, and the paths both need.
func ResolveGeminiLaunchExecutable() (GeminiLaunchExecutable, error) {
	entry, err := resolveLaunchExecutable("gemini", "gemini_launch_executable")
	if err != nil {
		return GeminiLaunchExecutable{}, err
	}
	return resolveGeminiLaunchFrom(entry.Path, exec.LookPath)
}

func resolveGeminiLaunchFrom(
	entry string,
	lookPath func(string) (string, error),
) (GeminiLaunchExecutable, error) {
	interpreterName, err := geminiShebangInterpreter(entry)
	if err != nil {
		return GeminiLaunchExecutable{}, err
	}
	if interpreterName == "" {
		// A native build: the executable alone is the closure.
		return GeminiLaunchExecutable{Path: entry, ReadPaths: []string{entry}}, nil
	}
	interpreter := interpreterName
	if !filepath.IsAbs(interpreter) {
		interpreter, err = lookPath(interpreterName)
		if err != nil {
			return GeminiLaunchExecutable{}, &NestedSandboxCapabilityError{
				Capability: "gemini_launch_executable",
				Detail: fmt.Sprintf(
					"Gemini entry point %q runs under %q, which is not on PATH: %v",
					entry, interpreterName, err),
			}
		}
	}
	node, err := inspectExecutableFile(interpreter, interpreterName, "gemini_launch_executable")
	if err != nil {
		return GeminiLaunchExecutable{}, err
	}
	readPaths := []string{node.Path}
	if root, ok := geminiPackageRoot(entry); ok {
		readPaths = append(readPaths, root)
		readPaths = append(readPaths, geminiHoistedDependencies(root)...)
	} else {
		// Not a recognizable npm layout: the bundle's own directory is the
		// closest closure that can be named.
		readPaths = append(readPaths, filepath.Dir(entry))
	}
	return GeminiLaunchExecutable{Interpreter: node.Path, Path: entry, ReadPaths: readPaths}, nil
}

// geminiShebangInterpreter names the interpreter entry's `#!` line runs, ""
// for a file without one. `/usr/bin/env [-S] node` names node for a PATH
// lookup; an absolute interpreter is returned as is. Only Node is accepted:
// the resolution exists to start a Node bundle, and an unrecognized
// interpreter is better refused than half-mounted.
func geminiShebangInterpreter(entry string) (string, error) {
	file, err := os.Open(entry)
	if err != nil {
		return "", fmt.Errorf("read Gemini entry point %q: %w", entry, err)
	}
	defer func() { _ = file.Close() }()
	line, err := bufio.NewReader(file).ReadString('\n')
	if err != nil && line == "" {
		return "", nil
	}
	if !strings.HasPrefix(line, "#!") {
		return "", nil
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return "", nil
	}
	interpreter := fields[0]
	if filepath.Base(interpreter) == "env" {
		interpreter = ""
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "-") {
				interpreter = field
				break
			}
		}
	}
	if !strings.HasPrefix(filepath.Base(interpreter), "node") {
		return "", &NestedSandboxCapabilityError{
			Capability: "gemini_launch_executable",
			Detail:     fmt.Sprintf("Gemini entry point %q is not a Node script (%q)", entry, strings.TrimSpace(line)),
		}
	}
	return interpreter, nil
}

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

// geminiHoistedDependencies returns the package's dependencies that npm placed
// beside it in the enclosing node_modules (a local or workspace install) rather
// than inside it (a global install). Only declared names that exist are
// returned, resolved to their real paths; a dependency nested in the package
// is already covered by the package root.
func geminiHoistedDependencies(root string) []string {
	_, deps, ok := readGeminiPackageManifest(root)
	if !ok {
		return nil
	}
	modules := filepath.Dir(filepath.Dir(root)) // node_modules/@google/gemini-cli
	if filepath.Base(modules) != "node_modules" {
		return nil
	}
	var out []string
	for _, dep := range deps {
		if filepath.IsAbs(dep) || strings.Contains(dep, "..") {
			continue
		}
		path := filepath.Join(modules, filepath.FromSlash(dep))
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if info, err := os.Stat(resolved); err == nil && info.IsDir() {
			out = append(out, resolved)
		}
	}
	return out
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
