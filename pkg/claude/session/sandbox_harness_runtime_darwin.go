//go:build darwin

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

var darwinClaudeRuntimeTempBase = "/private/tmp"

// tclaudeLayerHarnessRuntimeWriteDirs prepares writable host paths required by
// the harness before any tool subprocess starts. Claude Code stages Bash tool
// invocations below /private/tmp/claude-<uid> and writes per-command cwd state
// to unpredictable /tmp/claude-*-cwd files, independently of Darwin's standard
// $TMPDIR. Since /tmp resolves to /private/tmp on macOS, the outer Seatbelt
// layer must carry the canonical temp root as launch-contract authority even
// when Claude's own inner sandbox is disabled.
func tclaudeLayerHarnessRuntimeWriteDirs(harnessName string) ([]string, error) {
	if harnessName != harness.DefaultName {
		return nil, nil
	}
	base, err := filepath.EvalSymlinks(filepath.Clean(darwinClaudeRuntimeTempBase))
	if err != nil {
		return nil, fmt.Errorf(
			"canonicalize Claude runtime scratch base %q: %w",
			darwinClaudeRuntimeTempBase, err,
		)
	}
	path := filepath.Join(
		base,
		fmt.Sprintf("claude-%d", os.Geteuid()),
	)
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("prepare Claude runtime scratch root %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Claude runtime scratch root %q: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf(
			"claude runtime scratch root %q must be a real directory owned by uid %d",
			path, os.Geteuid(),
		)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return nil, fmt.Errorf("secure Claude runtime scratch root %q: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(path) {
		if err == nil {
			err = fmt.Errorf("resolved to %q", resolved)
		}
		return nil, fmt.Errorf("canonicalize Claude runtime scratch root %q: %w", path, err)
	}
	return []string{base, path}, nil
}
