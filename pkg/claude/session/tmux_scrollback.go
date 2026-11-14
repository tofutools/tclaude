package session

import (
	"strconv"
	"strings"

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

// CancelTmuxScrollback exits tmux copy mode on a managed pane when its
// native-scrollback client detaches. Copy mode belongs to the pane, not the
// client, so leaving it active would make the next attachment open in history
// and consume automated send-keys. The command is naturally best-effort: tmux
// returns an error when no mode is active, which is the desired no-op.
func CancelTmuxScrollback(tmuxSession string, h *harness.Harness) {
	if tmuxSession == "" || !h.WantsTmuxScrollback() {
		return
	}
	_ = clcommon.TmuxRun("send-keys", "-X", "-t",
		clcommon.ExactTarget(tmuxSession)+":0.0", "cancel")
}

// enableTmuxMouseScrollback is the underlying per-session mouse-mode toggle
// shared by ConfigureTmuxScrollback (gated on a harness's
// WantsTmuxScrollback) and a plain shell session (runNewShell), which always
// wants it — a bare shell has no self-managed scrollback of its own, so
// without tmux mouse mode the wheel does nothing in the pane.
func enableTmuxMouseScrollback(tmuxSession string) {
	_ = clcommon.TmuxRun("set-option", "-t", clcommon.ExactTarget(tmuxSession)+":", "mouse", "on")
}

// Scroll operations a browser terminal can ask of its managed pane
// (ScrollTmuxPane): further back, further forward, or back to live.
const (
	ScrollUp   = "up"
	ScrollDown = "down"
	ScrollLive = "live"
)

// maxScrollNotches bounds one request; a fling sends a notch at a time.
const maxScrollNotches = 50

// scrollLinesPerNotch matches tmux's copy-mode wheel binding.
const scrollLinesPerNotch = 5

// PaneScroll is where a pane's copy-mode is after a scroll request.
type PaneScroll struct {
	CopyMode bool `json:"copy_mode"`
	// Position is how many lines back from live the view is.
	Position int `json:"position"`
}

// ScrollTmuxPane scrolls a managed pane's history the way the mouse wheel
// does with tmux's mouse mode on, for a session whose mouse mode is off (Claude
// Code keeps its own scrollback, so its pane never turns tmux mouse on):
// up enters copy-mode (-e, so scrolling down to the bottom leaves it again)
// and scrolls five lines a notch, down scrolls back, live leaves copy-mode.
// A pane in another mode (choose-tree, say) is left alone. It reports where
// the pane is afterwards; a pane tmux cannot read reports live.
func ScrollTmuxPane(tmuxSession, op string, notches int) PaneScroll {
	if tmuxSession == "" {
		return PaneScroll{}
	}
	notches = max(1, min(notches, maxScrollNotches))
	target := clcommon.ExactTarget(tmuxSession) + ":0.0"
	lines := strconv.Itoa(notches * scrollLinesPerNotch)
	copyMode, inMode := paneCopyMode(target)
	switch op {
	case ScrollUp:
		if !copyMode {
			if inMode {
				break
			}
			if clcommon.TmuxRun("copy-mode", "-e", "-t", target) != nil {
				break
			}
		}
		_ = clcommon.TmuxRun("send-keys", "-t", target, "-X", "-N", lines, "scroll-up")
	case ScrollDown:
		if copyMode {
			_ = clcommon.TmuxRun("send-keys", "-t", target, "-X", "-N", lines, "scroll-down")
		}
	case ScrollLive:
		if copyMode {
			_ = clcommon.TmuxRun("send-keys", "-t", target, "-X", "cancel")
		}
	}
	return paneScrollState(target)
}

// LeaveTmuxCopyMode cancels a managed pane's copy-mode, if it is in it: a
// browser terminal that scrolled the pane leaves it live when it goes, or
// the pane would swallow the operator's keys and automated send-keys.
func LeaveTmuxCopyMode(tmuxSession string) {
	if tmuxSession == "" {
		return
	}
	target := clcommon.ExactTarget(tmuxSession) + ":0.0"
	if copyMode, _ := paneCopyMode(target); copyMode {
		_ = clcommon.TmuxRun("send-keys", "-t", target, "-X", "cancel")
	}
}

// paneCopyMode reads whether a pane is in copy-mode, and in any mode at all.
func paneCopyMode(target string) (copyMode, inMode bool) {
	out, err := clcommon.TmuxOutput("display-message", "-p", "-t", target, "#{==:#{pane_mode},copy-mode} #{pane_in_mode}")
	if err != nil {
		return false, false
	}
	f := strings.Fields(string(out))
	return len(f) == 2 && f[0] == "1", len(f) == 2 && f[1] == "1"
}

func paneScrollState(target string) PaneScroll {
	out, err := clcommon.TmuxOutput("display-message", "-p", "-t", target, "#{==:#{pane_mode},copy-mode} #{scroll_position}")
	if err != nil {
		return PaneScroll{}
	}
	f := strings.Fields(string(out))
	if len(f) == 0 || f[0] != "1" {
		return PaneScroll{}
	}
	state := PaneScroll{CopyMode: true}
	if len(f) > 1 {
		state.Position, _ = strconv.Atoi(f[1])
	}
	return state
}
