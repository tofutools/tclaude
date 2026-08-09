package session

import (
	"fmt"
	"log/slog"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

const tmuxDetachNormalizeOption = "@tclaude_detach_normalize"

// ConfigureTmuxDetachNormalization opts one managed session into the shared
// client-detached hook. A global append-only hook is necessary for detach
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
	// The check and append share one tmux command queue. This avoids duplicate
	// hooks when several attaches begin concurrently and self-repairs if an
	// operator later replaces the global hook array.
	condition := "#{==:#{m:*" + tmuxDetachNormalizeOption + "*,#{client-detached}},0}"
	hook := "if-shell -F '#{&&:#{==:#{" + tmuxDetachNormalizeOption +
		"},on},#{==:#{session_attached},0}}' '" +
		"run-shell -C \"" + tmuxDetachNormalizeCommand("=#{session_name}:") + "\"'"
	install := "set-hook -ag client-detached { " + hook + " }"
	_ = clcommon.TmuxCommand("if-shell", "-F", condition, install).Run()
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
