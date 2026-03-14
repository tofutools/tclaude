//go:build windows

package session

// focusTTY is a no-op on Windows (no tmux support).
func focusTTY(tty string) bool {
	return false
}
