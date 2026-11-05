//go:build windows

package session

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
// On Windows native, tmux sessions aren't typical, so this is a no-op.
func TryFocusAttachedSession(tmuxSession string) {
	// No-op for external focus attempts
}

// TryFocusAttachedSessionWithID exists for cross-platform parity
// with the Linux/WSL variant; native Windows is a no-op same as
// the existing helper.
func TryFocusAttachedSessionWithID(tmuxSession, _ string) {
	TryFocusAttachedSession(tmuxSession)
}
