//go:build linux || darwin

package host

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func prepareSandboxHarnessFloor(harness, nativeRoot string, policy model.SandboxPolicy, resources []SandboxProviderResource) ([]SandboxProviderResource, error) {
	var directories, files []string
	switch harness {
	case "claude":
		directories = []string{"hooks", "skills", "agents", "commands", "output-styles", "plugins", "workflows", "routines", "rules", "local", "cowork_plugins"}
		files = []string{"settings.json", "settings.local.json", "CLAUDE.md", "keybindings.json"}
	case "codex":
		directories = []string{"hooks", "prompts"}
		files = []string{"config.toml", "hooks.json", "AGENTS.md", "tclaude-agent.config.toml"}
	case "copilot":
		directories = []string{"hooks"}
		files = []string{"settings.json", "config.json", "mcp-config.json"}
	default:
		return nil, fmt.Errorf("sandbox configuration floor has no catalog for %q", harness)
	}
	declared := false
	for _, resource := range resources {
		if resource.Path == nativeRoot && resource.Access == model.SandboxFilesystemWrite {
			declared = true
		}
	}
	if !declared {
		return nil, fmt.Errorf("sandbox harness requires its declared writable native root")
	}
	switch policy.HarnessConfig {
	case model.SandboxHarnessConfigWrite:
		return nil, nil
	case model.SandboxHarnessConfigDefault, model.SandboxHarnessConfigRead:
	default:
		return nil, fmt.Errorf("unknown sandbox harness configuration access")
	}
	canonical, err := filepath.EvalSymlinks(nativeRoot)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	out := []SandboxProviderResource{}
	for index, name := range append(directories, files...) {
		path := filepath.Join(canonical, name)
		reopened := false
		for _, rule := range policy.Filesystem {
			guest := rule.GuestPath
			if guest == "" {
				guest = rule.HostPath
			}
			// Only an explicit write at this same native path opts it out.
			// A broad ancestor write does not remove the default floor.
			if rule.Access == model.SandboxFilesystemWrite && rule.HostPath == path && guest == path {
				reopened = true
			}
		}
		if reopened {
			continue
		}
		info, err := root.Lstat(name)
		if os.IsNotExist(err) && index < len(directories) {
			if err = root.Mkdir(name, 0700); err != nil && !os.IsExist(err) {
				return nil, err
			}
			info, err = root.Lstat(name)
		}
		if os.IsNotExist(err) {
			// Never invent configuration file content: absent and empty are
			// not interchangeable for the native harnesses.
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Match the retained dotfile-manager behavior. Resolving a link
			// would not protect its writable name from replacement.
			slog.Warn("sandbox configuration floor skips a symlinked entry", "harness", harness, "path", path)
			continue
		}
		if index < len(directories) && !info.IsDir() || index >= len(directories) && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("sandbox configuration entry has unexpected kind: %s", path)
		}
		out = append(out, SandboxProviderResource{Path: path, Access: model.SandboxFilesystemRead})
	}
	return out, nil
}
