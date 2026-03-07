//go:build darwin

package session

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
func TryFocusAttachedSession(tmuxSession string) {
	FocusTmuxSession(tmuxSession)
}

// FocusTmuxSession focuses the terminal window running a specific tmux session.
func FocusTmuxSession(tmuxSession string) bool {
	debug := os.Getenv("TOFU_DEBUG") != ""

	if debug {
		fmt.Printf("[debug] FocusTmuxSession: tmuxSession=%s\n", tmuxSession)
	}

	// Get the client tty for the target session
	tty := getTmuxClientTTY(tmuxSession)
	if debug {
		fmt.Printf("[debug] Target tty: %s\n", tty)
	}

	// Get the terminal app
	termApp := ""
	if tty != "" {
		termApp = terminalFromTTY(tty)
	}
	if debug {
		fmt.Printf("[debug] Terminal app: %q\n", termApp)
	}

	if termApp == "" {
		termApp = detectAnyRunningTerminal()
		if debug {
			fmt.Printf("[debug] Fallback terminal: %q\n", termApp)
		}
	}

	if termApp == "" {
		if debug {
			fmt.Println("[debug] No terminal app found")
		}
		return false
	}

	// Try terminal-specific focus
	if tty != "" {
		switch termApp {
		case "iTerm2":
			if focusITermTabByTTY(tty, debug) {
				return true
			}
		case "Terminal":
			if focusTerminalTabByTTY(tty, debug) {
				return true
			}
		}
	}

	// Fallback: just activate the app
	script := `tell application "` + termApp + `" to activate`
	if debug {
		fmt.Printf("[debug] Fallback: activating app\n")
	}
	cmd := exec.Command("osascript", "-e", script)
	return cmd.Run() == nil
}

// getTmuxClientTTY gets the tty of a client attached to a tmux session.
func getTmuxClientTTY(tmuxSession string) string {
	cmd := exec.Command("tmux", "list-clients", "-t", tmuxSession, "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return ""
	}
	return lines[0]
}

// focusITermTabByTTY focuses the iTerm2 tab that owns a specific tty.
func focusITermTabByTTY(tty string, debug bool) bool {
	// AppleScript to find and select the tab with matching tty
	script := fmt.Sprintf(`
tell application "iTerm2"
	activate
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is "%s" then
					select t
					select w
					return true
				end if
			end repeat
		end repeat
	end repeat
	return false
end tell
`, tty)

	if debug {
		fmt.Printf("[debug] Running iTerm2 AppleScript for tty %s\n", tty)
	}

	cmd := exec.Command("osascript", "-e", script)
	output, err := cmd.CombinedOutput()
	if debug {
		fmt.Printf("[debug] AppleScript output: %s, err: %v\n", strings.TrimSpace(string(output)), err)
	}
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// focusTerminalTabByTTY focuses the Terminal.app tab that owns a specific tty.
func focusTerminalTabByTTY(tty string, debug bool) bool {
	// AppleScript to find and select the tab with matching tty
	script := fmt.Sprintf(`
tell application "Terminal"
	activate
	repeat with w in windows
		repeat with t in tabs of w
			if tty of t is "%s" then
				set selected of t to true
				set index of w to 1
				return true
			end if
		end repeat
	end repeat
	return false
end tell
`, tty)

	if debug {
		fmt.Printf("[debug] Running Terminal.app AppleScript for tty %s\n", tty)
	}

	cmd := exec.Command("osascript", "-e", script)
	output, err := cmd.CombinedOutput()
	if debug {
		fmt.Printf("[debug] AppleScript output: %s, err: %v\n", strings.TrimSpace(string(output)), err)
	}
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// FocusOwnWindow attempts to focus the current process's terminal window.
// Uses AppleScript to activate the detected terminal application.
func FocusOwnWindow() bool {
	termApp := detectTerminalApp()
	if termApp == "" {
		return false
	}

	// Use AppleScript to activate the terminal
	script := `tell application "` + termApp + `" to activate`
	cmd := exec.Command("osascript", "-e", script)
	return cmd.Run() == nil
}

// GetOwnWindowTitle returns the title of the current terminal window.
func GetOwnWindowTitle() string {
	// Not easily available on macOS without terminal-specific APIs
	return ""
}

// detectTerminalApp tries to determine which terminal application we're running in.
func detectTerminalApp() string {
	// Check TERM_PROGRAM environment variable (set by most macOS terminals)
	termProgram := os.Getenv("TERM_PROGRAM")
	switch termProgram {
	case "Apple_Terminal":
		return "Terminal"
	case "iTerm.app":
		return "iTerm2"
	case "vscode":
		return "Visual Studio Code"
	case "Hyper":
		return "Hyper"
	case "Alacritty":
		return "Alacritty"
	case "kitty":
		return "kitty"
	case "WarpTerminal":
		return "Warp"
	}

	// Check if running inside tmux - look at parent processes
	if os.Getenv("TMUX") != "" {
		// We're in tmux, try to find the terminal from tmux client
		return detectTerminalFromTmux()
	}

	// Fallback: try common terminals
	terminals := []string{"iTerm2", "Terminal", "Alacritty", "kitty"}
	for _, term := range terminals {
		if isAppRunning(term) {
			return term
		}
	}

	return ""
}

// detectTerminalFromTmux tries to find which terminal is running the current tmux client.
func detectTerminalFromTmux() string {
	// Get the tmux client tty
	cmd := exec.Command("tmux", "display-message", "-p", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	tty := strings.TrimSpace(string(output))
	if tty == "" {
		return ""
	}

	return terminalFromTTY(tty)
}

// terminalFromTTY finds the terminal app owning a TTY.
func terminalFromTTY(tty string) string {
	// Try to find the process owning this tty using lsof
	cmd := exec.Command("lsof", "-t", tty)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	// Get all PIDs and walk up to find terminal
	pids := strings.Fields(string(output))
	for _, pid := range pids {
		term := findTerminalFromPID(pid)
		if term != "" {
			return term
		}
	}

	return ""
}

// findTerminalFromPID walks up the process tree to find a terminal app.
func findTerminalFromPID(pid string) string {
	debug := os.Getenv("TOFU_DEBUG") != ""
	visited := make(map[string]bool)

	for pid != "" && pid != "0" && pid != "1" && !visited[pid] {
		visited[pid] = true

		// Get process name
		cmd := exec.Command("ps", "-p", pid, "-o", "comm=")
		output, err := cmd.Output()
		if err != nil {
			if debug {
				fmt.Printf("[debug] ps comm= error for pid %s: %v\n", pid, err)
			}
			return ""
		}
		procName := strings.TrimSpace(string(output))

		if debug {
			fmt.Printf("[debug] PID %s -> %s\n", pid, procName)
		}

		term := terminalFromProcName(procName)
		if term != "" {
			if debug {
				fmt.Printf("[debug] Found terminal: %s\n", term)
			}
			return term
		}

		// Get parent PID
		cmd = exec.Command("ps", "-p", pid, "-o", "ppid=")
		output, err = cmd.Output()
		if err != nil {
			return ""
		}
		pid = strings.TrimSpace(string(output))
	}

	return ""
}

// terminalFromProcName maps process names to terminal app names.
func terminalFromProcName(procName string) string {
	switch {
	case strings.Contains(procName, "iTerm"):
		return "iTerm2"
	case strings.Contains(procName, "Terminal"):
		return "Terminal"
	case strings.Contains(procName, "Alacritty"):
		return "Alacritty"
	case strings.Contains(procName, "kitty"):
		return "kitty"
	case strings.Contains(procName, "Warp"):
		return "Warp"
	}
	return ""
}

// detectAnyRunningTerminal returns any running terminal app.
func detectAnyRunningTerminal() string {
	terminals := []string{"iTerm2", "Terminal", "Alacritty", "kitty", "Warp"}
	for _, term := range terminals {
		if isAppRunning(term) {
			return term
		}
	}
	return ""
}

// isAppRunning checks if an application is running on macOS.
func isAppRunning(appName string) bool {
	script := `tell application "System Events" to (name of processes) contains "` + appName + `"`
	cmd := exec.Command("osascript", "-e", script)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
}
