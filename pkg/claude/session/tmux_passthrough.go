package session

import (
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ConfigureTmuxPassthrough lets Copilot CLI deliver the terminal control
// sequences it deliberately wraps in tmux's DCS passthrough envelope. Copilot
// uses that path for OSC 52 clipboard writes while running under tmux; tmux
// drops the envelope while allow-passthrough is off.
//
// allow-passthrough is a window option, so target only this harness window.
// Do not make it global: the tclaude tmux server is shared by every harness
// (and may be shared with the operator's own tmux clients).
//
// Best-effort keeps launches working on older tmux versions which do not know
// the option. The only degradation there is the pre-existing missing terminal
// integration.
func ConfigureTmuxPassthrough(tmuxSession string, h *harness.Harness) {
	if h == nil || h.Name != harness.CopilotName {
		return
	}
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":",
		"allow-passthrough", "on").Run()
}
