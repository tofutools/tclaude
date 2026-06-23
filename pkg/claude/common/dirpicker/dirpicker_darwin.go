//go:build darwin

package dirpicker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// pick drives macOS's native folder chooser via osascript. `choose
// folder` returns an alias; `POSIX path of` converts it to a filesystem
// path. Cancelling the dialog makes osascript exit non-zero with
// "User canceled. (-128)" on stderr, which we map to ErrCanceled.
func pick(ctx context.Context, opts Options) (string, error) {
	title := opts.Title
	if title == "" {
		title = "Select a directory"
	}
	script := `POSIX path of (choose folder with prompt "` + escapeAppleScript(title) + `"`
	if opts.StartDir != "" {
		script += ` default location (POSIX file "` + escapeAppleScript(opts.StartDir) + `")`
	}
	script += `)`

	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := strings.ToLower(string(ee.Stderr))
			if strings.Contains(stderr, "-128") || strings.Contains(stderr, "user canceled") {
				return "", ErrCanceled
			}
			return "", fmt.Errorf("osascript: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// escapeAppleScript escapes s for interpolation inside an AppleScript
// double-quoted string literal.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}
