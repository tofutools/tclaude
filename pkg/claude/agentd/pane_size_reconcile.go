package agentd

import (
	"log/slog"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// normalizeUnattachedPaneSizes steers managed panes back to the canonical
// size once nobody is looking at them (TCL-1136).
//
// The tclaude server runs with tmux's `window-size latest`, so an attached
// client resizes the agent's window to itself — the right behaviour while
// the operator is watching — but tmux never reverts on detach: the window
// keeps the last client's size forever, and fleet sizes end up encoding
// attachment history instead of policy. This sweep closes that half: any
// live managed session with ZERO attached clients and a non-canonical size
// is resized back, and its `window-size latest` restored (resize-window
// switches a window to manual sizing, which would otherwise stop the NEXT
// client from getting its fit-to-client resize).
//
// The INFO line it emits doubles as forensics: a normalization firing is
// durable proof that SOMETHING attached to (or resized) that pane since the
// last sweep, which is exactly the question the Copilot stdin-wedge
// investigation could not answer from the logs of the day.
//
// Bounded and best-effort by design: one listing call per tick, one
// resize+set-option pair per divergent pane, all under tmuxCommandTimeout. A
// client attaching between the listing and the resize briefly fights the
// normalization; window-size latest is restored immediately after, so the
// client's next resize event wins. In flow tests the tmux sim ignores the
// -F format (bare names, no tabs), so every line fails the field check and
// the sweep is inert.
func normalizeUnattachedPaneSizes(states []*session.SessionState) {
	managed := make(map[string]string, len(states))
	for _, st := range states {
		if st.Status == session.StatusExited || st.TmuxSession == "" {
			continue
		}
		managed[st.TmuxSession] = st.ID
	}
	if len(managed) == 0 {
		return
	}
	out, err := tmuxOutputWithTimeout("list-sessions", "-F",
		"#{session_name}\t#{session_attached}\t#{window_width}\t#{window_height}")
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		name, attached := fields[0], fields[1]
		sessID, ok := managed[name]
		if !ok || attached != "0" {
			continue
		}
		w, werr := strconv.Atoi(fields[2])
		h, herr := strconv.Atoi(fields[3])
		if werr != nil || herr != nil {
			continue
		}
		if w == clcommon.CanonicalAgentPaneWidth && h == clcommon.CanonicalAgentPaneHeight {
			continue
		}
		target := clcommon.ExactTarget(name) + ":"
		if _, err := tmuxOutputWithTimeout("resize-window", "-t", target,
			"-x", strconv.Itoa(clcommon.CanonicalAgentPaneWidth),
			"-y", strconv.Itoa(clcommon.CanonicalAgentPaneHeight)); err != nil {
			slog.Warn("pane size normalize: resize-window failed",
				"session", sessID, "tmux_session", name, "error", err)
			continue
		}
		if _, err := tmuxOutputWithTimeout("set-option", "-w", "-t", target,
			"window-size", "latest"); err != nil {
			slog.Warn("pane size normalize: restoring window-size latest failed",
				"session", sessID, "tmux_session", name, "error", err)
		}
		slog.Info("pane size normalized after detach",
			"session", sessID, "tmux_session", name,
			"from", strconv.Itoa(w)+"x"+strconv.Itoa(h),
			"to", strconv.Itoa(clcommon.CanonicalAgentPaneWidth)+"x"+strconv.Itoa(clcommon.CanonicalAgentPaneHeight))
	}
}
