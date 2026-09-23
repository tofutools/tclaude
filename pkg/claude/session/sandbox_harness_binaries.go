package session

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// tclaudeLayerInstalledHarnessNames are the executables every tclaude-layer
// launch exposes, whichever harness it runs, so an agent can start a one-shot
// `tclaude run --harness <other>` from inside its sandbox.
var tclaudeLayerInstalledHarnessNames = []string{"claude", "codex", "opencode", "copilot"}

// Seams for tests, which must not depend on the host's installs.
var (
	tclaudeLayerHarnessLookPath    = exec.LookPath
	tclaudeLayerResolveNativeCodex = harness.ResolveCodexLaunchExecutable
)

// TclaudeLayerHarnessBinaries is the read-only surface that makes the
// installed harness executables usable inside tclaude's sandbox.
type TclaudeLayerHarnessBinaries struct {
	// ReadPaths are the resolved executables or package roots to reopen.
	ReadPaths []string
	// EntryPoints are the PATH spellings (often symlinks such as
	// ~/.local/bin/codex) that must resolve inside the sandbox as on the host.
	EntryPoints []string
}

// ResolveTclaudeLayerHarnessBinaries finds every installed harness on PATH and
// returns what a sandbox needs to run it. A harness that is not installed is
// skipped: this is best-effort exposure, never a launch requirement. A Node.js
// launcher also brings its package root and the node executable.
func ResolveTclaudeLayerHarnessBinaries() TclaudeLayerHarnessBinaries {
	var out TclaudeLayerHarnessBinaries
	seen := map[string]bool{}
	addRead := func(path string) {
		path = filepath.Clean(path)
		if path == "" || path == "." || seen[path] || tclaudeLayerStaticOSRootProvides(path) {
			return
		}
		seen[path] = true
		out.ReadPaths = append(out.ReadPaths, path)
	}
	addEntry := func(name string) (string, bool) {
		spelling, err := tclaudeLayerHarnessLookPath(name)
		if err != nil {
			return "", false
		}
		spelling, err = filepath.Abs(spelling)
		if err != nil {
			return "", false
		}
		resolved, err := filepath.EvalSymlinks(spelling)
		if err != nil {
			return "", false
		}
		if spelling != resolved {
			out.EntryPoints = append(out.EntryPoints, spelling)
		}
		return resolved, true
	}
	needsNode := false
	for _, name := range tclaudeLayerInstalledHarnessNames {
		resolved, ok := addEntry(name)
		if !ok {
			continue
		}
		if root := nodePackageRoot(resolved); root != "" {
			addRead(root)
		} else {
			addRead(resolved)
		}
		if isNodeScript(resolved) {
			needsNode = true
		}
		if name == harness.CodexName {
			// A standalone native Codex needs its sibling runtime closure (rg,
			// bwrap); an npm install is already covered by its package root.
			if native, err := tclaudeLayerResolveNativeCodex(); err == nil && native.RuntimeRoot != "" {
				addRead(native.RuntimeRoot)
			}
		}
	}
	if needsNode {
		if resolved, ok := addEntry("node"); ok {
			addRead(resolved)
		}
	}
	return out
}

// nodePackageRoot returns the npm package directory containing path, or ""
// when path is not inside node_modules.
func nodePackageRoot(path string) string {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		if filepath.Base(parent) == "node_modules" {
			return dir
		}
		if strings.HasPrefix(filepath.Base(parent), "@") && filepath.Base(filepath.Dir(parent)) == "node_modules" {
			return dir
		}
	}
}

// isNodeScript reports whether path starts with a shebang naming node.
func isNodeScript(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	line, err := bufio.NewReader(file).ReadSlice('\n')
	if err != nil && len(line) == 0 {
		return false
	}
	return bytes.HasPrefix(line, []byte("#!")) && bytes.Contains(line, []byte("node"))
}

// tclaudeLayerEntryPointAliases recreates each harness PATH spelling that is
// a symlink, so a bare `codex` resolves inside a constructed root, or beneath
// a hidden home, exactly as it does on the host.
func tclaudeLayerEntryPointAliases(entryPoints []string) []sandboxpolicy.MountAlias {
	var out []sandboxpolicy.MountAlias
	for _, entry := range entryPoints {
		aliases, err := sandboxpolicy.MountAliasesForPath(entry)
		if err != nil {
			continue
		}
		out = append(out, aliases...)
	}
	return out
}
