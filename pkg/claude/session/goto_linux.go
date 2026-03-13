//go:build linux

package session

// focusTTY focuses the terminal window owning the given TTY.
// On Linux, falls back to the generic FocusTmuxSession path via xdotool.
func focusTTY(tty string) bool {
	// TODO: could use xdotool or similar
	return false
}
