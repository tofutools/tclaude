//go:build linux

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OpenWithCommand opens a new terminal window running the given command.
func OpenWithCommand(command string) error {
	if isWSL() {
		return openWSL(command)
	}
	return openLinuxNative(command)
}

// isWSL detects if we're running in Windows Subsystem for Linux.
func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// openWSL opens a terminal in WSL environment via Windows Terminal or cmd.exe.
func openWSL(command string) error {
	// Try Windows Terminal first (most common on modern Windows)
	if wtPath := findWindowsTerminal(); wtPath != "" {
		// wt.exe new-tab wsl -e sh -c "command"
		return exec.Command(wtPath, "new-tab", "wsl", "-e", "sh", "-c", command).Start()
	}
	// Fallback: open cmd.exe which then runs wsl
	return exec.Command("cmd.exe", "/c", "start", "wsl", "-e", "sh", "-c", command).Start()
}

// findWindowsTerminal looks for Windows Terminal executable.
func findWindowsTerminal() string {
	// Try PATH first
	if path, err := exec.LookPath("wt.exe"); err == nil {
		return path
	}

	// Common paths for Windows Terminal
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("LOGNAME")
	}

	basePaths := []string{
		filepath.Join("/mnt/c/Users", user, "AppData/Local/Microsoft/WindowsApps/wt.exe"),
	}

	for _, p := range basePaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Try glob for WindowsApps
	pattern := filepath.Join("/mnt/c/Users", user, "AppData/Local/Microsoft/WindowsApps/wt.exe")
	if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
		return matches[0]
	}

	return ""
}

// openLinuxNative opens a terminal on native Linux.
func openLinuxNative(command string) error {
	// Terminals to try in order of preference
	terminals := []struct {
		cmd  string
		args func(string) []string
	}{
		{"x-terminal-emulator", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
		{"gnome-terminal", func(c string) []string { return []string{"--", "sh", "-c", c} }},
		{"konsole", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
		{"xfce4-terminal", func(c string) []string { return []string{"-e", "sh -c '" + c + "'"} }},
		{"alacritty", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
		{"kitty", func(c string) []string { return []string{"--", "sh", "-c", c} }},
		{"xterm", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
	}

	for _, t := range terminals {
		if _, err := exec.LookPath(t.cmd); err == nil {
			args := t.args(command)
			return exec.Command(t.cmd, args...).Start()
		}
	}
	return fmt.Errorf("no terminal emulator found")
}
