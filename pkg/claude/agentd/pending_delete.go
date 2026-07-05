package agentd

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// handleDashboardPendingDeleteAPI cleans up a pending spawn keyed on its
// LABEL — the escape hatch for a spawn that is stuck in the "Pending"
// virtual group and will never enrol (chiefly a Codex agent wedged behind
// a startup gate the operator has given up on: an untrusted dir, a
// new-config prompt, an abandoned OpenAI-auth modal). A pending spawn has
// no conv-id and no agent_id yet, so the conv-keyed retire / stop / delete
// endpoints cannot reach it; the only two things it owns are its
// pending_spawns row and a session row (both keyed by the label), plus a
// possibly-live tmux pane. This handler tears down all three:
//
//	POST /api/pending/delete/{label}
//
//  1. kill the tmux pane if it is still alive (the gated harness process),
//  2. delete the session row,
//  3. delete the pending_spawns row.
//
// It is the deliberate counterpart to the pending sweeper's happy path
// (enrol → deletePendingSpawnRow): where the sweeper promotes a spawn once
// its gate clears, this discards one whose gate never will.
//
// Same threat model as handleDashboardPendingFocusAPI and the rest of
// /api/* — the dashboard cookie + Origin pin is the human-consent layer
// (see dashboard_edit.go). A pending spawn is not an enrolled agent, so it
// carries no permission slug and there is no /v1 twin to funnel through;
// the cookie gate is the same level the focus and retire-by-drag paths use.
func handleDashboardPendingDeleteAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/pending/delete/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/pending/delete/{label}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/pending/delete/{label}/"+parts[1], http.StatusNotFound)
		return
	}
	label := parts[0]
	if u, err := url.PathUnescape(label); err == nil {
		label = u
	}

	// Confirm the label is actually a pending spawn before tearing anything
	// down. The sweeper may have enrolled + deleted the row in the moment
	// between the snapshot the operator clicked and this request; that race
	// is benign — the agent is now a normal enrolled agent reachable via the
	// conv-keyed retire path, and the dashboard's 2s re-poll moves it out of
	// the pending list — so a 404 here is the correct, self-healing answer
	// (mirrors handleDashboardPendingFocusAPI).
	p, err := db.GetPendingSpawn(label)
	if err != nil {
		http.Error(w, "pending lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "no pending spawn "+label+" (already enrolled or cleaned up)", http.StatusNotFound)
		return
	}

	// Kill the tmux pane if it is still alive — this is the gated harness
	// process the operator is discarding. A dead pane (operator closed it,
	// the spawn crashed) just skips this; the row cleanup below still runs.
	if sess, err := db.LoadSession(label); err == nil && sess != nil && sess.TmuxSession != "" {
		alive, _ := session.LiveTmuxSessions()
		if _, ok := alive[sess.TmuxSession]; ok {
			if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(sess.TmuxSession)).Run(); err != nil {
				slog.Warn("pending delete: kill-session failed",
					"label", label, "tmux", sess.TmuxSession, "error", err)
			}
		}
	}

	// Delete the session row, then the pending row. Order doesn't matter —
	// both are keyed by the label and neither references the other — and a
	// partial failure is self-healing: a leftover pending row is re-swept
	// (its pane is now dead, so it lingers only until the pane-liveness
	// sweep clears it), and a leftover session row is a harmless orphan.
	if err := db.DeleteSession(label); err != nil {
		slog.Warn("pending delete: delete session row failed", "label", label, "error", err)
	}
	if err := db.DeletePendingSpawn(label); err != nil {
		http.Error(w, "delete pending row: "+err.Error(), http.StatusInternalServerError)
		return
	}

	slog.Info("pending delete: cleaned up pending spawn", "label", label)
	writeJSON(w, http.StatusOK, map[string]string{"label": label})
}
