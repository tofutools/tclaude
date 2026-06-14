package agentd

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// handleDashboardPendingFocusAPI opens a terminal window ATTACHED to a
// pending spawn's tmux pane, keyed on the spawn LABEL. A pending agent
// (the pending_spawns table — JOH-205 inc2) has a live pane but no
// conv-id yet, so the conv-keyed focus / open-window endpoints cannot
// reach it. Opening its pane is exactly what lets the operator clear the
// startup gate that is holding the conv-id back (an untrusted dir, a
// new-hooks-config prompt, the OpenAI auth modal); once the gate is
// cleared the spawn takes its first turn, its conv-id materialises, and
// the pending_spawn sweeper promotes it into an enrolled agent.
//
//	POST /api/pending/focus/{label}
//
// It reuses the same openTerminal + openAttachCmd primitive as the spawn
// auto-focus path (lifecycle.go) and handleDashboardOpenWindowAPI, only
// keyed on the label instead of a conv-id. Window-only: like its
// conv-keyed twin it never touches the agent PROCESS — it just gives the
// detached pane a window.
//
// Same threat model as the rest of /api/* — the dashboard cookie + Origin
// pin is the human-consent layer (see dashboard_edit.go). Focusing a
// window is a human-desktop operation with no /v1 twin and no permission
// slug, so there is no shared permission-checked handler to funnel
// through — the same rationale as handleDashboardOpenWindowAPI and the
// bulk /api/agent-windows endpoint.
func handleDashboardPendingFocusAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/pending/focus/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/pending/focus/{label}", http.StatusNotFound)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/pending/focus/{label}/"+parts[1], http.StatusNotFound)
		return
	}
	label := parts[0]
	if u, err := url.PathUnescape(label); err == nil {
		label = u
	}

	// Confirm the label is actually a pending spawn before opening a
	// window for it — this keeps the endpoint scoped to its purpose
	// instead of a generic "attach to any label" surface. The sweeper
	// may have enrolled + deleted the row in the moment between the
	// snapshot the operator clicked and this request; that race is
	// benign — the agent is now a normal enrolled agent reachable via the
	// conv-keyed focus path, and the dashboard's 2s re-poll moves it out
	// of the pending list — so a 404 here is the correct, self-healing
	// answer.
	p, err := db.GetPendingSpawn(label)
	if err != nil {
		http.Error(w, "pending lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "no pending spawn "+label+" (already enrolled or cleaned up)", http.StatusNotFound)
		return
	}

	// The pending spawn must still carry a LIVE tmux pane to attach to —
	// a row whose pane has died (operator closed it, or the spawn
	// crashed) has nothing to focus. The dashboard already disables the
	// button for a dead pane (online=false); this is the matching
	// server-side guard, the same offline→404 boundary as the per-agent
	// hide / jump endpoints.
	sess, err := db.LoadSession(label)
	if err != nil || sess == nil || sess.TmuxSession == "" {
		http.Error(w, "no tmux pane for pending spawn "+label, http.StatusNotFound)
		return
	}
	alive, _ := session.LiveTmuxSessions()
	if _, ok := alive[sess.TmuxSession]; !ok {
		http.Error(w, "pending spawn "+label+" has no live tmux pane", http.StatusNotFound)
		return
	}

	if err := openTerminal(openAttachCmd(label)); err != nil {
		http.Error(w, "open window: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"label": label})
}
