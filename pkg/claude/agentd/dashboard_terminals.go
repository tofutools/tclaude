package agentd

import "net/http"

// terminalsPageHTML is terminals.html, read once at init — the standalone
// /terminals page handleDashboardTerminals serves.
var terminalsPageHTML = mustReadFS(dashboardAssetsFS, "terminals.html")

// handleDashboardTerminals serves the standalone /terminals page, behind the
// same auth gate as the dashboard root. The multiplexer itself now lives in the
// dashboard's own "Terminals" tab (js/terminals-tab.js); this page exists only
// for the per-terminal "⧉ tab" pop-out (/terminals?solo=1 — one terminal in its
// own OS/browser window, seeded via the URL hash; see js/terminals.js). The
// page only ever loads /static/* assets and connects to the existing
// /api/term-ws and /api/open-window-ws WebSocket endpoints, which carry their
// own auth checks.
func handleDashboardTerminals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// Remote (mTLS + passphrase) requests are authenticated at the remote
	// listener's boundary — serve directly, like handleDashboardRoot does.
	if dashboardPreAuthed(r) {
		writeTerminalsPage(w)
		return
	}
	// Loopback: require the session cookie the human's browser got when it
	// opened the dashboard at /. This page is always reached FROM the
	// dashboard, so the cookie is present; if it's missing or stale (e.g. a
	// daemon restart minted a fresh token), bounce to / to re-authenticate
	// rather than dead-ending on a plain 403.
	if c, err := r.Cookie(dashboardCookieName); err == nil && dashboardSessionToken != "" && c.Value == dashboardSessionToken {
		writeTerminalsPage(w)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func writeTerminalsPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(terminalsPageHTML)
}
