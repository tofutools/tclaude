//go:build darwin

package session

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
)

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
func TryFocusAttachedSession(tmuxSession string) {
	FocusTmuxSession(tmuxSession, "")
}

// TryFocusAttachedSessionWithID focuses the terminal hosting the session, and
// if no terminal is attached, spawns a new one running `tclaude session attach <id>`.
func TryFocusAttachedSessionWithID(tmuxSession, sessionID string) {
	FocusTmuxSession(tmuxSession, sessionID)
}

// FocusTmuxSession focuses the terminal window running a specific tmux session.
// When no client is currently attached and sessionID is non-empty, a new terminal
// window is opened that runs `tclaude session attach <sessionID>`.
func FocusTmuxSession(tmuxSession, sessionID string) bool {
	debug := os.Getenv("TCLAUDE_DEBUG") != ""

	if debug {
		fmt.Fprintf(cliStdout, "[debug] FocusTmuxSession: tmuxSession=%s sessionID=%s\n", tmuxSession, sessionID)
	}

	// Get the client tty for the target session
	tty := getTmuxClientTTY(tmuxSession)
	if debug {
		fmt.Fprintf(cliStdout, "[debug] Target tty: %s\n", tty)
	}

	raiseOnly := focusRaiseOnlyFn()

	// No client attached and we know the session ID → spawn a new terminal
	// that attaches via `tclaude session attach <id>` so the wrapper sets
	// TCLAUDE_SESSION_ID + window title before execing tmux. Skipped when
	// focus.raise_only is configured: the user has opted out of the
	// open-on-focus side effect (use the explicit "open window" action).
	if !raiseOnly && tty == "" && sessionID != "" {
		if openTerminalAttachingSession(sessionID, debug) {
			return true
		}
	}

	// Raise-only with no attached client: there is no window for THIS
	// session to raise, and we won't open one — so stop here rather than
	// falling through to detectAnyRunningTerminal + activate, which would
	// raise an arbitrary unrelated terminal (the very pop-up raise_only
	// exists to suppress). Mirrors the Linux focusLinuxNoClients arm.
	if raiseOnly && tty == "" {
		if debug {
			fmt.Fprintln(cliStdout, "[debug] raise_only and no attached client; nothing to raise")
		}
		return false
	}

	// Get the terminal app
	termApp := ""
	if tty != "" {
		termApp = terminalFromTTY(tty)
	}
	if debug {
		fmt.Fprintf(cliStdout, "[debug] Terminal app: %q\n", termApp)
	}

	if termApp == "" {
		termApp = detectAnyRunningTerminal()
		if debug {
			fmt.Fprintf(cliStdout, "[debug] Fallback terminal: %q\n", termApp)
		}
	}

	if termApp == "" {
		if debug {
			fmt.Fprintln(cliStdout, "[debug] No terminal app found")
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
		fmt.Fprintf(cliStdout, "[debug] Fallback: activating app\n")
	}
	cmd := exec.Command("osascript", "-e", script)
	return cmd.Run() == nil
}

// getTmuxClientTTY gets the tty of a client attached to a tmux session.
func getTmuxClientTTY(tmuxSession string) string {
	ttys, err := tmuxcmd.Local().ClientTTYs(tmuxcmd.Session(tmuxSession))
	if err != nil || len(ttys) == 0 {
		return ""
	}
	return ttys[0]
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
		fmt.Fprintf(cliStdout, "[debug] Running iTerm2 AppleScript for tty %s\n", tty)
	}

	cmd := exec.Command("osascript", "-e", script)
	output, err := cmd.CombinedOutput()
	if debug {
		fmt.Fprintf(cliStdout, "[debug] AppleScript output: %s, err: %v\n", strings.TrimSpace(string(output)), err)
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
		fmt.Fprintf(cliStdout, "[debug] Running Terminal.app AppleScript for tty %s\n", tty)
	}

	cmd := exec.Command("osascript", "-e", script)
	output, err := cmd.CombinedOutput()
	if debug {
		fmt.Fprintf(cliStdout, "[debug] AppleScript output: %s, err: %v\n", strings.TrimSpace(string(output)), err)
	}
	return err == nil && strings.TrimSpace(string(output)) == "true"
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
	debug := os.Getenv("TCLAUDE_DEBUG") != ""
	visited := make(map[string]bool)

	for pid != "" && pid != "0" && pid != "1" && !visited[pid] {
		visited[pid] = true

		// Get process name
		cmd := exec.Command("ps", "-p", pid, "-o", "comm=")
		output, err := cmd.Output()
		if err != nil {
			if debug {
				fmt.Fprintf(cliStdout, "[debug] ps comm= error for pid %s: %v\n", pid, err)
			}
			return ""
		}
		procName := strings.TrimSpace(string(output))

		if debug {
			fmt.Fprintf(cliStdout, "[debug] PID %s -> %s\n", pid, procName)
		}

		term := terminalFromProcName(procName)
		if term != "" {
			if debug {
				fmt.Fprintf(cliStdout, "[debug] Found terminal: %s\n", term)
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
