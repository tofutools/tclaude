package session

import (
	"fmt"
	"log/slog"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

const tmuxDetachNormalizeHookIndex = "9001136"

// ConfigureTmuxDetachNormalization installs the client-detached hook that
// restores one managed session to the canonical detached size. The hook is
// necessary for detach paths with no returning tclaude caller, notably
// switch-client from an agentd TUI and clients attached directly with tmux.
//
// The hook is SESSION-scoped with the session name baked in as a literal, and
// its body is plain tmux commands — deliberately not the earlier global
// `run-shell -C "… #{session_name} …"` formulation. That formulation
// segfaulted the shared tmux server (observed on tmux 3.7b): run-shell defers
// its command through a callback attributed to the detaching client, and when
// several clients detach at once while other commands (for example
// NormalizeTmuxPaneAfterDetach from a returning caller) interleave in the
// server's command queue, the callback can run after that client is freed —
// a use-after-free in cmdq_next → proc_get_peer_uid that takes down every
// agent on the server. Directly-queued hook commands do not take that
// deferred path, and the literal target also fixes a mistargeting bug the
// global hook had: with several simultaneous detaches, #{session_name}
// resolution after client destruction could land every resize on one session.
//
// Installation is idempotent: every call overwrites the same tclaude-owned
// hook-array slot on the same session, and the hook dies with the session, so
// nothing global is left behind. Operator hooks at other indices are never
// touched.
func ConfigureTmuxDetachNormalization(tmuxSession string) {
	// The name is interpolated into a hook body (a tmux command string), so it
	// is an injection sink: gate it on the managed-name charset every producer
	// (ValidateSessionLabel, sanitizeTmuxName, UUID-prefix fallback) already
	// guarantees, rather than trusting future name sources to keep doing so.
	// A name that fails the gate simply gets no hook; the returning-caller
	// normalize and agentd's periodic reconciliation still cover it.
	if tmuxSession == "" || !isSafeTmuxHookName(tmuxSession) {
		return
	}
	target := clcommon.ExactTarget(tmuxSession) + ":"
	hook := "if-shell -F -t " + target + " '#{==:#{session_attached},0}' '" +
		tmuxDetachNormalizeCommand(target) + "'"
	_ = clcommon.TmuxCommand("set-hook", "-t", target,
		"client-detached["+tmuxDetachNormalizeHookIndex+"]", hook).Run()
	// Long-lived tmux servers may still carry the crash-prone global hook an
	// earlier tclaude installed; drop it so an upgrade actually ends the
	// segfaults without a server restart. Best-effort and idempotent.
	_ = clcommon.TmuxCommand("set-hook", "-gu",
		"client-detached["+tmuxDetachNormalizeHookIndex+"]").Run()
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

// isSafeTmuxHookName reports whether a session name stays within the managed
// [A-Za-z0-9_-] charset and so cannot break out of the quoted hook body it is
// interpolated into.
func isSafeTmuxHookName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func tmuxDetachNormalizeCommand(target string) string {
	return fmt.Sprintf("resize-window -t %s -x %d -y %d ; set-option -w -t %s window-size latest",
		target, clcommon.CanonicalAgentPaneWidth, clcommon.CanonicalAgentPaneHeight, target)
}
