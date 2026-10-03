package session

import clcommon "github.com/tofutools/tclaude/pkg/claude/common"

// ConfigureTmuxPassthrough lets applications deliver terminal control sequences
// wrapped in tmux's DCS passthrough envelope, including OSC 52 clipboard writes.
// This is the default for every tclaude-managed harness window, so new harnesses
// do not need to opt in to terminal integration.
//
// Scope the option to this window: the shared tmux server and unrelated windows
// retain their existing policy. Use on (visible panes), not all (hidden panes).
// Best-effort keeps launches working on older tmux versions without the option.
func ConfigureTmuxPassthrough(tmuxSession string) {
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":",
		"allow-passthrough", "on").Run()
}
