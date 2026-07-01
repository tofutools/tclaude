package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expandTilde rewrites a leading "~" or "~/" in path to the current
// user's home directory. Anything else (including "~user" forms and
// embedded tildes) is returned unchanged — we only support the common
// "my home" shorthand. agentd runs as the human, so the home it
// expands to is the human's own home directory.
func expandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// resolveSpawnCwd validates and normalises a working directory supplied
// to a spawn/clone request. It:
//
//   - returns ("", nil) for an empty input — callers treat that as
//     "use the daemon's default cwd", same as before;
//   - expands a leading "~" / "~/" to the human's home directory, so a
//     dashboard input like "~/git/myproject" works;
//   - makes the path absolute (relative paths resolve against the
//     daemon's cwd);
//   - requires the path to exist and be a directory.
//
// The existence check is the point of this function. Before it, a bad
// cwd sailed past the daemon into a detached `tclaude session new`
// subprocess that failed silently; the caller then waited out the 30s
// conv-id poll and got a confusing gateway-timeout. Validating up front
// turns that into an immediate, actionable 400.
func resolveSpawnCwd(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	expanded := expandTilde(raw)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("invalid working directory %q: %v", raw, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("working directory does not exist: %s", abs)
		}
		return "", fmt.Errorf("cannot access working directory %s: %v", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory: %s", abs)
	}
	return abs, nil
}

// launchDirMissing classifies a resolved launch directory for the resume
// path, where the recorded cwd may have been deleted since the agent last
// ran (a worktree pruned, a scratch dir cleaned up). Unlike resolveSpawnCwd
// — which rejects a missing dir outright — resume needs to distinguish the
// recoverable "dir was deleted" case from a real access error so it can
// offer to recreate the empty dir rather than spawn a child that wedges at
// startup with no clear error.
//
//   - a blank cwd (the daemon's default, always present) is never "missing";
//   - a path that does not exist yields missing=true, err=nil — the caller
//     may recreate it empty and continue;
//   - a stat error other than not-exist (e.g. a permission failure), or a
//     path that exists but is not a directory, yields err != nil — those are
//     not the empty-recreate case and the caller should surface them rather
//     than silently mkdir over them.
func launchDirMissing(cwd string) (missing bool, err error) {
	if strings.TrimSpace(cwd) == "" {
		return false, nil
	}
	info, statErr := os.Stat(cwd)
	if statErr == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("launch path is not a directory: %s", cwd)
		}
		return false, nil
	}
	if os.IsNotExist(statErr) {
		return true, nil
	}
	return false, fmt.Errorf("cannot access launch directory %s: %v", cwd, statErr)
}

// resolveGroupDefaultCwd validates a working directory being stored as
// a group's default spawn dir. Unlike resolveSpawnCwd it does NOT
// require the directory to exist — a default may legitimately be set
// before the directory is created, and the spawn-time resolveSpawnCwd
// performs the existence check at the point it actually matters. It:
//
//   - returns ("", nil) for empty input — that clears the default;
//   - expands a leading "~" / "~/" to the human's home directory;
//   - REQUIRES the result to be absolute. A relative default would
//     resolve against whatever cwd the daemon happens to run in,
//     which is meaningless, so it's rejected rather than silently
//     made absolute against the daemon's cwd.
func resolveGroupDefaultCwd(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	expanded := expandTilde(raw)
	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("default working directory must be an absolute path: %q", raw)
	}
	return filepath.Clean(expanded), nil
}
