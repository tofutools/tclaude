package agentd

import (
	"net/http"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleDashboardBrowserNotifications serves the browser-delivery queue to
// the dashboard, which raises each item as a Web Notification.
//
// Cursor protocol, no server-side consumption: the caller passes the last
// id it saw as `since` and gets back everything newer plus a new `cursor`.
// Nothing is deleted on read, so several open tabs (and several browsers)
// each see every notification — the same way each would have seen one OS
// banner. A caller with NO cursor yet omits `since` and receives an empty
// batch plus the current head, so a freshly-opened dashboard starts at
// "from now on" instead of replaying a backlog.
//
// GET /api/browser-notifications?since=<id>
//
//	→ {"cursor": 42, "notifications": [{id, session_id, title, body, created_at}, …]}
func handleDashboardBrowserNotifications(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// enabled tells the client whether browser delivery is configured at
	// all, so a browser that once granted permission can back off to a
	// slow heartbeat instead of polling every 3s forever for a channel
	// the operator left switched off. Advisory only — the queue is empty
	// in that case anyway; this just saves the round-trips.
	enabled := false
	if cfg, err := config.Load(); err == nil {
		enabled = cfg.Notifications.DeliverToBrowser()
	}

	raw := r.URL.Query().Get("since")
	if raw == "" {
		// No cursor: hand back the head and nothing else.
		head, err := db.LatestBrowserNotificationID()
		if err != nil {
			http.Error(w, "read browser notifications: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":       enabled,
			"cursor":        head,
			"notifications": []db.BrowserNotification{},
		})
		return
	}

	since, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || since < 0 {
		http.Error(w, "since must be a non-negative integer id", http.StatusBadRequest)
		return
	}

	items, head, err := db.ListBrowserNotificationsSince(since)
	if err != nil {
		http.Error(w, "read browser notifications: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []db.BrowserNotification{}
	}
	// Never hand back a cursor BEHIND the caller's own — a pruned queue
	// (MAX(id) gone) would otherwise rewind it and replay whatever lands next.
	if head < since {
		head = since
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       enabled,
		"cursor":        head,
		"notifications": items,
	})
}
