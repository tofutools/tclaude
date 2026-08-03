package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TclaudeTmuxSocketDir returns the private directory holding tclaude's named
// tmux socket (`tmux -L <socket>`, see TmuxSocketName). tmux uses
// $TMUX_TMPDIR/tmux-UID when that
// variable is set and /tmp/tmux-UID otherwise. Blocking the directory covers
// both the current socket and a server created after policy rendering.
//
// Windows is not a supported tclaude target; the project targets Linux and
// macOS, where os.Getuid is available.
func TclaudeTmuxSocketDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("TMUX_TMPDIR"))
	if base == "" {
		base = "/tmp"
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("TMUX_TMPDIR %q is not absolute", base)
	}
	base, err := filepath.EvalSymlinks(filepath.Clean(base))
	if err != nil {
		return "", fmt.Errorf("resolve tmux socket base %q: %w", base, err)
	}
	return filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid())), nil
}
