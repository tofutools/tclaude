package harness

import (
	"fmt"
	"runtime"
	"sort"

	"github.com/pelletier/go-toml/v2"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// CodexManagedFilesystemRule is one launch-independent filesystem entry in
// tclaude's managed Codex permission profile.
type CodexManagedFilesystemRule struct {
	Path   string
	Access string
}

// CodexManagedBaselineFilesystemRules returns the canonical filesystem layer
// that every managed Codex launch profile renders before cwd/Git grants and
// sandbox-profile rules are added. It renders through the same builder used by
// EnsureCodexAgentLaunchProfileForRules, so callers never mistake the installed
// tclaude-agent.config.toml convenience file for launch authority: that file
// may be absent or stale, while every launch gets a freshly generated
// tclaude-agent-<launch-id>.config.toml.
func CodexManagedBaselineFilesystemRules() ([]CodexManagedFilesystemRule, error) {
	socketPath, privateStateDir, _, err := codexAgentSandboxPaths()
	if err != nil {
		return nil, err
	}
	content, err := codexAgentProfileContentForRules(
		CodexAgentProfile,
		socketPath,
		privateStateDir,
		CodexSandboxRules{},
		sandboxpolicy.NetworkAccessInherit,
		runtime.GOOS,
	)
	if err != nil {
		return nil, err
	}
	var config struct {
		Permissions map[string]struct {
			Filesystem map[string]string `toml:"filesystem"`
		} `toml:"permissions"`
	}
	if err := toml.Unmarshal([]byte(content), &config); err != nil {
		return nil, fmt.Errorf("decode generated managed Codex profile: %w", err)
	}
	profile, ok := config.Permissions[CodexAgentProfile]
	if !ok {
		return nil, fmt.Errorf("generated managed Codex profile is missing permissions.%s", CodexAgentProfile)
	}
	paths := make([]string, 0, len(profile.Filesystem))
	for path := range profile.Filesystem {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]CodexManagedFilesystemRule, 0, len(paths))
	for _, path := range paths {
		out = append(out, CodexManagedFilesystemRule{Path: path, Access: profile.Filesystem[path]})
	}
	return out, nil
}
