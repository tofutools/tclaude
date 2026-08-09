package session

import (
	"fmt"
	"log/slog"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

const (
	tmuxDetachNormalizeOption    = "@tclaude_detach_normalize"
	tmuxDetachNormalizeHookIndex = "9001136"
)

// ConfigureTmuxDetachNormalization opts one managed session into the shared
// client-detached hook. A global hook in a dedicated array slot is necessary for detach
// paths with no returning tclaude caller, notably switch-client from an
// agentd TUI and clients attached directly with tmux. The per-session option
// keeps unrelated sessions on the shared tclaude tmux server untouched.
func ConfigureTmuxDetachNormalization(tmuxSession string) {
	if tmuxSession == "" {
		return
	}
	target := clcommon.ExactTarget(tmuxSession) + ":"
	_ = clcommon.TmuxCommand("set-option", "-t", target,
		tmuxDetachNormalizeOption, "on").Run()
	ensureTmuxDetachNormalizeHook()
}

func ensureTmuxDetachNormalizeHook() {
	// A fixed, tclaude-owned hook-array slot makes concurrent installation
	// idempotent: every caller overwrites the same entry. An absent-check plus
	// append is not atomic across tmux client command queues — concurrent
	// launches can all observe absence before any queued append executes.
	// Setting one explicit slot preserves every operator hook at other indices
	// and self-repairs if this slot is later removed or replaced.
	hook := "if-shell -F '#{&&:#{==:#{" + tmuxDetachNormalizeOption +
		"},on},#{==:#{session_attached},0}}' '" +
		"run-shell -C \"" + tmuxDetachNormalizeCommand("=#{session_name}:") + "\"'"
	_ = clcommon.TmuxCommand("set-hook", "-g",
		"client-detached["+tmuxDetachNormalizeHookIndex+"]", hook).Run()
}

// NormalizeTmuxPaneAfterDetach restores a managed window to the canonical
// detached size if its last client has left. The predicate and mutation run in
// one server-side tmux command queue, so a concurrent attach cannot slip
// between a client-count subprocess and resize the new viewer's live window.
//
// Best-effort by design. The client-detached hook handles paths without a
// returning caller, and agentd's periodic pane-size reconciliation remains the
// recovery path for a transient tmux failure or a replaced hook.
func NormalizeTmuxPaneAfterDetach(tmuxSession string) {
	if tmuxSession == "" {
		return
	}
	target := clcommon.ExactTarget(tmuxSession) + ":"
	if err := clcommon.TmuxCommand("if-shell", "-F", "-t", target,
		"#{==:#{session_attached},0}", tmuxDetachNormalizeCommand(target)).Run(); err != nil {
		slog.Warn("pane size normalize after detach failed",
			"tmux_session", tmuxSession, "error", err)
	}
}

func tmuxDetachNormalizeCommand(target string) string {
	return fmt.Sprintf("resize-window -t %s -x %d -y %d ; set-option -w -t %s window-size latest",
		target, clcommon.CanonicalAgentPaneWidth, clcommon.CanonicalAgentPaneHeight, target)
}
