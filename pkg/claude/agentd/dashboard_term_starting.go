package agentd

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// An agent terminal opened right after a spawn, resume or relaunch can arrive
// before the launch has a live tmux pane: `session new` writes the session row
// (launch intent) before it creates the pane, and a dashboard spawn reserves
// the agent before either. That is a normal waiting state, not a failure, so
// the agent terminal routes answer it with a distinct "starting" close that the
// browser (terminals-core.js TERMINAL_STARTING) retries on its own with a short
// backoff. Only a launch that is still recent counts: a row that has sat
// without a pane for longer than terminalStartWindow is an agent that did not
// start, and the browser settles on its ordinary Disconnected state.
const (
	// termCloseStarting says the agent's launch is still in progress; the
	// reason is a short human phrase.
	termCloseStarting = 4005
	// termCloseNotRunning says the agent has no live pane and is not starting.
	// It replaces a bare 404 before the upgrade, which a browser WebSocket
	// cannot read, so the pane can say why it is disconnected.
	termCloseNotRunning = 4006
)

// terminalStartWindow is how long after a launch began an agent without a
// live pane still counts as starting.
const terminalStartWindow = 30 * time.Second

// terminalNow is the clock the starting checks read; tests pin it.
var terminalNow = time.Now

// sessionRowLaunching reports whether row is a launch that began inside the
// start window and has not exited. Its pane may or may not be up yet; callers
// that need "not attachable yet" check the pane themselves. CreatedAt is the
// launch's own time: the launch row write replaces it on a reused row.
func sessionRowLaunching(row *db.SessionRow, now time.Time) bool {
	if row == nil || row.TmuxSession == "" || row.Status == session.StatusExited || row.CreatedAt.IsZero() {
		return false
	}
	age := now.Sub(row.CreatedAt)
	return age >= -terminalStartWindow && age < terminalStartWindow
}

// pendingSpawnLaunching reports whether a dashboard spawn reserved agentID and
// has not produced its session row yet. A reservation still marked launching
// is the spawn core's own "in progress" (agentd ages stale ones out); an older
// unmarked one counts only inside the start window.
func pendingSpawnLaunching(agentID string, now time.Time) bool {
	if agentID == "" {
		return false
	}
	pending, err := db.GetPendingSpawnByAgentID(agentID)
	if err != nil || pending == nil {
		return false
	}
	if pending.Launching {
		return true
	}
	created, err := time.Parse(time.RFC3339Nano, pending.CreatedAt)
	return err == nil && now.Sub(created) < terminalStartWindow
}

// agentTerminalStarting reports whether the agent behind convID (or agentID,
// when the selector named an agent whose conversation is not bound yet) has a
// launch in progress: a recent, unexited session row, or a pending spawn.
func agentTerminalStarting(convID, agentID string) bool {
	now := terminalNow()
	if convID != "" {
		rows, err := db.FindSessionsByConvID(convID)
		if err != nil {
			return false
		}
		for _, row := range rows {
			if sessionRowLaunching(row, now) {
				return true
			}
		}
		if agentID == "" {
			agentID, _ = db.AgentIDForConv(convID)
		}
	}
	return pendingSpawnLaunching(agentID, now)
}

// labelTerminalStarting is agentTerminalStarting for a label-keyed terminal
// (spawn focus): the label's own row, or the pending spawn holding the label.
func labelTerminalStarting(label, agentID string) bool {
	now := terminalNow()
	if row, err := db.LoadSession(label); err == nil && sessionRowLaunching(row, now) {
		return true
	}
	if pending, err := db.GetPendingSpawn(label); err == nil && pending != nil {
		if agentID == "" || pending.AgentID == "" || pending.AgentID == agentID {
			return true
		}
	}
	return pendingSpawnLaunching(agentID, now)
}

// selectorAgentID returns selector when it names an active agent, for an
// agent selector that does not resolve to a conversation yet.
func selectorAgentID(selector string) string {
	if !strings.HasPrefix(selector, db.AgentIDPrefix) {
		return ""
	}
	if a, err := db.GetAgent(selector); err != nil || a == nil || !a.Active() {
		return ""
	}
	return selector
}

// closeTerminalBeforeAttach upgrades the request and closes the socket at once
// with code and reason, for an agent terminal that has nothing to attach to
// yet. A refusal before the upgrade would reach the browser only as an opaque
// failed handshake.
func closeTerminalBeforeAttach(w http.ResponseWriter, r *http.Request, code int, reason string) {
	conn, err := upgradeTerminalWebSocket(w, r)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	slog.Debug("browser terminal not attached", "path", r.URL.Path, "code", code, "reason", reason)
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}
