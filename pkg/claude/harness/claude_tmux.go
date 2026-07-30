package harness

import (
	"fmt"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

type claudeTmuxHostControlSandbox struct{}

func (claudeTmuxHostControlSandbox) PrepareLaunch(spec SpawnSpec) (SpawnSpec, error) {
	return PrepareClaudeSandboxLaunch(spec)
}

// ClaudeTmuxSocketDenyPath resolves tclaude's named tmux server socket. The
// boundary is intentionally socket-specific: agents may run tmux against a
// private socket they own, but must not connect to the server hosting tclaude
// agent panes.
func ClaudeTmuxSocketDenyPath() (string, error) {
	dir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return "", fmt.Errorf("resolve Claude tmux socket deny path: %w", err)
	}
	return filepath.Join(dir, clcommon.TmuxSocketName), nil
}

// PrepareClaudeSandboxLaunch adds the tclaude tmux server socket to every
// tclaude-managed Claude sandbox launch. "inherit" still leaves the operator's
// enabled/disabled choice alone: the deny becomes active only when their
// sandbox is enabled. An explicit "off" is the visible escape hatch and emits
// no irrelevant filesystem settings.
//
// Callers use this before BuildCommand / BuildAskArgv because resolving the
// host path can fail and those pure renderer interfaces deliberately do not
// return errors.
func PrepareClaudeSandboxLaunch(spec SpawnSpec) (SpawnSpec, error) {
	if strings.TrimSpace(spec.SandboxMode) == ClaudeSandboxOff {
		return spec, nil
	}
	path, err := ClaudeTmuxSocketDenyPath()
	if err != nil {
		return SpawnSpec{}, err
	}
	path = filepath.Clean(path)
	denies := append([]string(nil), spec.SandboxDenyDirs...)
	for _, existing := range denies {
		if filepath.Clean(strings.TrimSpace(existing)) == path {
			spec.SandboxDenyDirs = denies
			return spec, nil
		}
	}
	spec.SandboxDenyDirs = append(denies, path)
	return spec, nil
}
