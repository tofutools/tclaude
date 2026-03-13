package session

import (
	"os"
	"runtime"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// ConfigureTmuxKeybindings sets up keybindings on the tclaude tmux server
// for session navigation. Prefix-less so they work without Ctrl+b.
//
// Only enabled on macOS with iTerm2 or Terminal.app, since the goto command
// uses AppleScript to focus terminal windows/tabs.
//
// Current bindings:
//   - Shift+Right → goto next session
//   - Shift+Left  → goto prev session
func ConfigureTmuxKeybindings() {
	if runtime.GOOS != "darwin" {
		return
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app", "Apple_Terminal":
		// Supported terminals
	default:
		return
	}

	// Prefix-less (-n): Shift+Arrow to switch sessions
	// #{session_name} is expanded by tmux to the current session name
	clcommon.TmuxCommand("bind-key", "-n", "S-Right",
		"run-shell", "-b", "TCLAUDE_SESSION_ID=#{session_name} tclaude session goto next 2>/dev/null").Run()
	clcommon.TmuxCommand("bind-key", "-n", "S-Left",
		"run-shell", "-b", "TCLAUDE_SESSION_ID=#{session_name} tclaude session goto prev 2>/dev/null").Run()
}
