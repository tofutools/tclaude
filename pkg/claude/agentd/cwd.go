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
