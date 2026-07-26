package agentd

import (
	"log/slog"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// sandboxRestartTmuxHandoff keeps attached tmux clients alive across the gap
// where a same-conversation restart has no agent pane to target. Unlike a
// reincarnation, the replacement reuses the old tmux name, so it cannot be
// created before the old pane exits. A short-lived bridge session gives the
// clients somewhere to remain attached during that gap.
type sandboxRestartTmuxHandoff struct {
	holdingSession string
}

const sandboxRestartHoldingCommand = "printf '\\033[2J\\033[HRestarting agent with updated sandbox settings...\\n'; while :; do sleep 3600; done"

// beginSandboxRestartTmuxHandoff moves clients attached to oldTmux onto a
// temporary bridge session. It is best-effort, matching reincarnation's client
// carry-over: failure to preserve a terminal must not block the operator's
// requested sandbox transition.
func beginSandboxRestartTmuxHandoff(oldTmux string) *sandboxRestartTmuxHandoff {
	if strings.TrimSpace(oldTmux) == "" {
		return nil
	}
	holding := "restart-" + strings.TrimPrefix(generateSpawnLabel(), "spwn-")
	if err := clcommon.TmuxCommand(
		"new-session", "-d", "-s", holding,
		"sh", "-c", sandboxRestartHoldingCommand,
	).Run(); err != nil {
		slog.Warn("sandbox restart: could not create tmux client bridge",
			"from", oldTmux, "bridge", holding, "error", err)
		return nil
	}
	if switched := switchTmuxClients(oldTmux, holding); switched == 0 {
		killSandboxRestartTmuxBridge(holding)
		return nil
	}
	return &sandboxRestartTmuxHandoff{holdingSession: holding}
}

// finish moves every client still on the bridge to targetTmux, then removes
// the bridge. Empty targetTmux is the failed-restart cleanup path: there is no
// useful agent pane to switch to, so the unmanaged bridge must not leak.
func (h *sandboxRestartTmuxHandoff) finish(targetTmux string) int {
	if h == nil || h.holdingSession == "" {
		return 0
	}
	holding := h.holdingSession
	h.holdingSession = ""
	switched := 0
	if strings.TrimSpace(targetTmux) != "" {
		switched = switchTmuxClients(holding, targetTmux)
	}
	killSandboxRestartTmuxBridge(holding)
	return switched
}

// finishForConv resolves the replacement pane only after the synchronous
// resume wrapper has returned. At that point session new has committed both
// the live tmux session and its row.
func (h *sandboxRestartTmuxHandoff) finishForConv(convID string) int {
	if sess := pickAliveSession(convID); sess != nil {
		return h.finish(sess.TmuxSession)
	}
	return h.finish("")
}

func killSandboxRestartTmuxBridge(holding string) {
	if err := clcommon.TmuxCommand(
		"kill-session", "-t", clcommon.ExactTarget(holding),
	).Run(); err != nil {
		slog.Warn("sandbox restart: could not remove tmux client bridge",
			"bridge", holding, "error", err)
	}
}
