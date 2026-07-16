package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LivePaneCwd returns tmux's view of a live pane process's physical working
// directory. It follows the cwd inode the pane actually entered rather than a
// launch pathname whose symlink may have been retargeted later.
func LivePaneCwd(tmuxSession string) (string, error) {
	out, err := TmuxCommand("display-message", "-p", "-t", ExactTarget(tmuxSession)+":", "#{pane_current_path}").Output()
	if err != nil {
		return "", fmt.Errorf("query live pane working directory: %w", err)
	}
	cwd := strings.TrimSpace(string(out))
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("query live pane working directory: tmux returned %q", cwd)
	}
	physical, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve live pane working directory %s: %w", cwd, err)
	}
	info, err := os.Stat(physical)
	if err != nil {
		return "", fmt.Errorf("stat live pane working directory %s: %w", physical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("live pane working directory is not a directory: %s", physical)
	}
	return physical, nil
}
