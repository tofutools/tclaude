package harness

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// NestedSandboxExecutable freezes the exact harness entry point a tclaude-layer
// launch runs, plus (for Codex) the runtime directory that must travel with it.
// The name is historical: it was introduced for the retired stacked sandbox,
// whose nested engine probe resolved the same entry point.
type NestedSandboxExecutable struct {
	Path        string
	RuntimeRoot string
}

// NestedSandboxCapabilityError names the exact missing capability when a
// harness entry point cannot be resolved for tclaude's own sandbox.
type NestedSandboxCapabilityError struct {
	Capability string
	Detail     string
}

func (err *NestedSandboxCapabilityError) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("missing capability %s: %s", err.Capability, err.Detail)
}

// ResolveClaudeLaunchExecutable freezes the Claude entry point before a
// tclaude-layer launch enters its filesystem namespace. Native Claude installs
// commonly place only a symlink on PATH while the versioned executable lives
// beneath ~/.local/share, which a constructed root intentionally omits.
func ResolveClaudeLaunchExecutable() (NestedSandboxExecutable, error) {
	return resolveLaunchExecutable("claude", "claude_launch_executable")
}

// ResolveCodexLaunchExecutable freezes the native Codex executable and its
// tightly coupled runtime directory before a tclaude-layer launch enters its
// filesystem namespace. Codex installations commonly expose a symlink or npm
// launcher from PATH while the executable that must run lives elsewhere.
func ResolveCodexLaunchExecutable() (NestedSandboxExecutable, error) {
	launcher, err := resolveLaunchExecutable("codex", "codex_launch_executable")
	if err != nil {
		return NestedSandboxExecutable{}, err
	}
	return resolveCodexNativeExecutable(launcher, "codex_launch_executable")
}

func resolveLaunchExecutable(name, capability string) (NestedSandboxExecutable, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("model-free %s entry point is not on PATH: %v", name, err),
		}
	}
	return inspectExecutableFile(path, name, capability)
}

func inspectExecutableFile(path, name, capability string) (NestedSandboxExecutable, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("canonicalize %s entry point: %v", name, err),
		}
	}
	if evaluated, evalErr := filepath.EvalSymlinks(path); evalErr == nil {
		path = evaluated
	}
	info, err := os.Stat(path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("inspect %s entry point: %v", name, err),
		}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail: fmt.Sprintf(
				"%s entry point %q is not an executable regular file", name, path),
		}
	}
	return NestedSandboxExecutable{Path: path}, nil
}

func resolveCodexNativeExecutable(
	launcher NestedSandboxExecutable,
	capability string,
) (NestedSandboxExecutable, error) {
	file, err := os.Open(launcher.Path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("open Codex entry point %q: %v", launcher.Path, err),
		}
	}
	var magic [4]byte
	_, readErr := io.ReadFull(file, magic[:])
	closeErr := file.Close()
	if readErr == nil && string(magic[:]) == "\x7fELF" {
		runtimeRoot, rootErr := codexNativeRuntimeRoot(launcher.Path)
		if rootErr != nil {
			return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
				Capability: capability,
				Detail:     rootErr.Error(),
			}
		}
		launcher.RuntimeRoot = runtimeRoot
		return launcher, nil
	}
	if closeErr != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     fmt.Sprintf("close Codex entry point %q: %v", launcher.Path, closeErr),
		}
	}
	packageName, targetTriple, err := codexLinuxNativeTarget()
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     err.Error(),
		}
	}
	nativePath, err := findCodexNPMNativeBackend(
		launcher.Path,
		packageName,
		targetTriple,
	)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail: fmt.Sprintf(
				"resolve the native backend behind Codex entry point %q: %v",
				launcher.Path,
				err,
			),
		}
	}
	native, err := inspectExecutableFile(
		nativePath, "codex native backend", capability)
	if err != nil {
		return NestedSandboxExecutable{}, err
	}
	runtimeRoot, err := codexNativeRuntimeRoot(native.Path)
	if err != nil {
		return NestedSandboxExecutable{}, &NestedSandboxCapabilityError{
			Capability: capability,
			Detail:     err.Error(),
		}
	}
	native.RuntimeRoot = runtimeRoot
	return native, nil
}

func codexLinuxNativeTarget() (string, string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "@openai/codex-linux-x64", "x86_64-unknown-linux-musl", nil
	case "arm64":
		return "@openai/codex-linux-arm64", "aarch64-unknown-linux-musl", nil
	default:
		return "", "", fmt.Errorf(
			"the official Codex npm launcher has no pinned Linux backend for architecture %s",
			runtime.GOARCH,
		)
	}
}

func findCodexNPMNativeBackend(
	launcherPath, packageName, targetTriple string,
) (string, error) {
	packageParts := strings.Split(packageName, "/")
	if len(packageParts) != 2 || packageParts[0] != "@openai" ||
		strings.TrimSpace(packageParts[1]) == "" {
		return "", fmt.Errorf("invalid Codex platform package %q", packageName)
	}
	launcherPath = filepath.Clean(launcherPath)
	packageRoot := filepath.Dir(filepath.Dir(launcherPath))
	scopeRoot := filepath.Dir(filepath.Dir(packageRoot))
	if filepath.Base(filepath.Dir(launcherPath)) != "bin" ||
		filepath.Base(packageRoot) != "codex" ||
		filepath.Base(filepath.Dir(packageRoot)) != "@openai" ||
		filepath.Base(scopeRoot) != "node_modules" {
		return "", fmt.Errorf(
			"codex launcher %q is outside a recognized node_modules/@openai/codex package",
			launcherPath,
		)
	}
	candidates := []string{
		filepath.Join(
			packageRoot,
			"node_modules",
			packageParts[0],
			packageParts[1],
			"vendor",
			targetTriple,
			"bin",
			"codex",
		),
		filepath.Join(
			scopeRoot,
			packageParts[0],
			packageParts[1],
			"vendor",
			targetTriple,
			"bin",
			"codex",
		),
	}
	for _, candidate := range candidates {
		if evaluated, evalErr := filepath.EvalSymlinks(candidate); evalErr == nil {
			relative, relErr := filepath.Rel(scopeRoot, evaluated)
			insideScope := relErr == nil && relative != ".." &&
				!strings.HasPrefix(relative, ".."+string(filepath.Separator))
			if info, statErr := os.Stat(evaluated); insideScope &&
				statErr == nil && info.Mode().IsRegular() {
				return evaluated, nil
			}
		}
	}
	return "", fmt.Errorf("official npm platform package %s is missing", packageName)
}

func codexNativeRuntimeRoot(nativePath string) (string, error) {
	if filepath.Base(nativePath) != "codex" ||
		filepath.Base(filepath.Dir(nativePath)) != "bin" {
		return "", fmt.Errorf(
			"codex native backend %q has no recognized runtime layout",
			nativePath,
		)
	}
	root := filepath.Dir(filepath.Dir(nativePath))
	for _, relative := range []string{
		"codex-package.json",
		filepath.Join("codex-path", "rg"),
		filepath.Join("codex-resources", "bwrap"),
	} {
		path := filepath.Join(root, relative)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf(
				"codex native runtime closure is missing %q",
				path,
			)
		}
	}
	return root, nil
}
