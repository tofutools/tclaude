package session

import (
	"log/slog"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// NormalizeTmuxPaneAfterDetach restores a managed window to the canonical
// detached size as soon as its last client leaves. The session-attached check
// matters when several clients are watching the same pane: one viewer leaving
// must not resize the window underneath the viewers that remain.
//
// Best-effort by design. The daemon's periodic pane-size reconciliation is the
// recovery path for a transient tmux failure or an attachment path that exits
// without reaching its normal cleanup.
func NormalizeTmuxPaneAfterDetach(tmuxSession string) {
	if tmuxSession == "" {
		return
	}
	target := clcommon.ExactTarget(tmuxSession) + ":"
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", target,
		"#{session_attached}\t#{window_width}\t#{window_height}\t#{window-size}").Output()
	if err != nil {
		return
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(fields) != 4 || fields[0] != "0" {
		return
	}
	w, werr := strconv.Atoi(fields[1])
	h, herr := strconv.Atoi(fields[2])
	if werr != nil || herr != nil {
		return
	}
	needResize := w != clcommon.CanonicalAgentPaneWidth || h != clcommon.CanonicalAgentPaneHeight
	if !needResize && fields[3] != "manual" {
		return
	}
	if needResize {
		if err := clcommon.TmuxCommand("resize-window", "-t", target,
			"-x", strconv.Itoa(clcommon.CanonicalAgentPaneWidth),
			"-y", strconv.Itoa(clcommon.CanonicalAgentPaneHeight)).Run(); err != nil {
			slog.Warn("pane size normalize after detach: resize-window failed",
				"tmux_session", tmuxSession, "error", err)
			return
		}
	}
	// resize-window selects manual sizing. Restore latest even when the window
	// was already canonical but stuck in manual mode, so the next client fits.
	if err := clcommon.TmuxCommand("set-option", "-w", "-t", target,
		"window-size", "latest").Run(); err != nil {
		slog.Warn("pane size normalize after detach: restoring window-size latest failed",
			"tmux_session", tmuxSession, "error", err)
		return
	}
	if needResize {
		slog.Info("pane size normalized immediately after detach",
			"tmux_session", tmuxSession,
			"from", strconv.Itoa(w)+"x"+strconv.Itoa(h),
			"to", strconv.Itoa(clcommon.CanonicalAgentPaneWidth)+"x"+strconv.Itoa(clcommon.CanonicalAgentPaneHeight))
	} else {
		slog.Info("pane size normalize after detach: repaired manual window sizing",
			"tmux_session", tmuxSession)
	}
}
