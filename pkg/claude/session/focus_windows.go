//go:build windows

package session

import (
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	user32           = syscall.NewLazyDLL("user32.dll")
	getConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	setForegroundWnd = user32.NewProc("SetForegroundWindow")
	showWindow       = user32.NewProc("ShowWindow")
	isIconic         = user32.NewProc("IsIconic")
)

const (
	swRestore = 9
)

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
// On Windows native, tmux sessions aren't typical, so this is a no-op.
func TryFocusAttachedSession(tmuxSession string) {
	// No-op for external focus attempts
}

// FocusOwnWindow attempts to focus the current process's console window.
// This is called from within the session (via hooks) and should succeed
// because Windows allows a process to focus its own windows.
func FocusOwnWindow() bool {
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return false
	}

	// If window is minimized, restore it first
	iconic, _, _ := isIconic.Call(hwnd)
	if iconic != 0 {
		showWindow.Call(hwnd, swRestore)
	}

	// Try to bring to foreground
	ret, _, _ := setForegroundWnd.Call(hwnd)
	return ret != 0
}

// GetOwnWindowTitle returns the title of the current console window.
// Useful for debugging window identification issues.
func GetOwnWindowTitle() string {
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return ""
	}

	getWindowTextW := user32.NewProc("GetWindowTextW")
	buf := make([]uint16, 256)
	getWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf)
}
