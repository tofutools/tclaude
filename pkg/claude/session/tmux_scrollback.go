package session

import (
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ConfigureTmuxScrollback enables tmux mouse mode for a single session when
// its harness leans on tmux for scroll-back history (Codex CLI) rather than
// rendering its own (Claude Code). With mouse mode on, the wheel scrolls the
// pane's copy-mode buffer — which is exactly the history a Codex agent's TUI
// would otherwise scroll off the top of an unscrollable pane (JOH-213).
//
// It is scoped to THIS session (-t <session>), never global (-g): the
// `-L tclaude` server is shared by every session, so a global toggle would
// turn mouse mode on for Claude Code panes too (where it fights CC's own
// mouse handling) and would behave like editing the user's tmux config. A
// per-session set-option touches neither. Harnesses that render their own
// scrollback leave WantsTmuxScrollback false and this is a no-op.
//
// Best-effort and silent, mirroring the sibling set-titles options in
// session.runNew: if the option can't be set the pane simply falls back to
// keyboard copy-mode (Ctrl+b [), so there is nothing actionable to surface.
func ConfigureTmuxScrollback(tmuxSession string, h *harness.Harness) {
	if !h.WantsTmuxScrollback() {
		return
	}
	enableTmuxMouseScrollback(tmuxSession)
}

// enableTmuxMouseScrollback is the underlying per-session mouse-mode toggle
// shared by ConfigureTmuxScrollback (gated on a harness's
// WantsTmuxScrollback) and a plain shell session (runNewShell), which always
// wants it — a bare shell has no self-managed scrollback of its own, so
// without tmux mouse mode the wheel does nothing in the pane.
func enableTmuxMouseScrollback(tmuxSession string) {
	_ = clcommon.TmuxCommand("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":", "mouse", "on").Run()
}
