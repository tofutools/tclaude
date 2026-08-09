package agentd

import (
	"log/slog"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// agentRestartTmuxHandoff keeps attached tmux clients alive across the gap
// where a same-conversation restart has no agent pane to target. Unlike a
// reincarnation, the replacement reuses the old tmux name, so it cannot be
// created before the old pane exits. A short-lived bridge session gives the
// clients somewhere to remain attached during that gap.
type agentRestartTmuxHandoff struct {
	holdingSession string
}

// Five minutes comfortably covers the normal 10-second shutdown grace and
// synchronous resume, but bounds the unmanaged bridge if agentd exits after
// parking clients or its explicit kill-session cleanup fails.
const agentRestartHoldingCommand = "printf '\\033[2J\\033[HRestarting agent with updated settings...\\n'; sleep 300"

// beginAgentRestartTmuxHandoff moves clients attached to oldTmux onto a
// temporary bridge session. It is best-effort, matching reincarnation's client
// carry-over: failure to preserve a terminal must not block the operator's
// requested restart.
func beginAgentRestartTmuxHandoff(oldTmux string) *agentRestartTmuxHandoff {
	if strings.TrimSpace(oldTmux) == "" {
		return nil
	}
	clients := tmuxClientTTYs(oldTmux)
	if len(clients) == 0 {
		return nil
	}
	if err := session.RequireExternalTmuxServer(); err != nil {
		slog.Warn("agent restart: external tmux runtime is unavailable", "error", err)
		return nil
	}
	holding := "restart-" + strings.TrimPrefix(generateSpawnLabel(), "spwn-")
	if err := clcommon.TmuxCommand(session.ExternalTmuxNoStartArgs(
		"new-session", "-d", "-s", holding,
		"-x", strconv.Itoa(clcommon.CanonicalAgentPaneWidth),
		"-y", strconv.Itoa(clcommon.CanonicalAgentPaneHeight),
		"sh", "-c", agentRestartHoldingCommand,
	)...).Run(); err != nil {
		slog.Warn("agent restart: could not create tmux client bridge",
			"from", oldTmux, "bridge", holding, "error", err)
		return nil
	}
	// User tmux configuration can inherit remain-on-exit=on into this pane.
	// Override it pane-locally so the command's five-minute exit really removes
	// the bridge even if agentd is no longer around to run kill-session.
	if err := clcommon.TmuxCommand(
		"set-option", "-p", "-t", clcommon.ExactTarget(holding)+":",
		"remain-on-exit", "off",
	).Run(); err != nil {
		slog.Warn("agent restart: could not bound tmux client bridge lifetime",
			"from", oldTmux, "bridge", holding, "error", err)
		killAgentRestartTmuxBridge(holding)
		return nil
	}
	if switched := switchTmuxClientTTYs(clients, oldTmux, holding); switched == 0 {
		killAgentRestartTmuxBridge(holding)
		return nil
	}
	return &agentRestartTmuxHandoff{holdingSession: holding}
}

// finish moves every client still on the bridge to targetTmux, then removes
// the bridge. Empty targetTmux is the failed-restart cleanup path: there is no
// useful agent pane to switch to, so the unmanaged bridge must not leak.
func (h *agentRestartTmuxHandoff) finish(targetTmux string) int {
	if h == nil || h.holdingSession == "" {
		return 0
	}
	holding := h.holdingSession
	h.holdingSession = ""
	switched := 0
	if strings.TrimSpace(targetTmux) != "" {
		switched = switchTmuxClients(holding, targetTmux)
	}
	killAgentRestartTmuxBridge(holding)
	return switched
}

// finishForConv resolves the replacement pane only after the synchronous
// resume wrapper has returned. At that point session new has committed both
// the live tmux session and its row.
func (h *agentRestartTmuxHandoff) finishForConv(convID string) int {
	if sess := pickAliveSession(convID); sess != nil {
		return h.finish(sess.TmuxSession)
	}
	return h.finish("")
}

func killAgentRestartTmuxBridge(holding string) {
	if err := clcommon.TmuxCommand(
		"kill-session", "-t", clcommon.ExactTarget(holding),
	).Run(); err != nil {
		slog.Warn("agent restart: could not remove tmux client bridge",
			"bridge", holding, "error", err)
	}
}
