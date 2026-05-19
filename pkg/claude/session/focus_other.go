//go:build !linux

package session

// IsXdotoolInstalled / IsKdotoolInstalled / LinuxFocusToolName are
// Linux-only at runtime — xdotool is X11, kdotool is KWin DBus, and
// macOS / Windows have their own focus paths (AppleScript on macOS,
// PowerShell on Windows). These stubs exist so callers in
// platform-agnostic packages (setup/setup.go's reportLinuxFocusTools,
// which is gated by runtime.GOOS == "linux" at call time but still
// needs to LINK on every platform) can reference them without a
// per-platform build-tag dance at every call site. The real probes
// live in focus_linux.go.
//
// History: setup.go used to inline its own isXdotoolInstalled, with
// the duplication noted by the PR #199 cold review. Consolidating
// onto session's exported probes was the fix — but session's probes
// only existed for linux, so non-linux builds broke. This file is
// the missing piece (caught by main's CI after merge, not by mine).

func IsXdotoolInstalled() bool   { return false }
func IsKdotoolInstalled() bool   { return false }
func LinuxFocusToolName() string { return "" }
