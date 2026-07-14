package agentd

import "net/http"

// terminalsPageHTML is terminals.html, read once at init — the standalone
// /terminals page handleDashboardTerminals serves.
var terminalsPageHTML = mustReadFS(dashboardAssetsFS, "terminals.html")

// handleDashboardTerminals serves the /terminals route, behind the same auth
// gate as the dashboard root. It has two jobs, discriminated by the ?solo query:
//
//   - /terminals?solo=1 — the standalone popout page (js/terminals.js): the
//     per-terminal "⧉ tab" pop-out, one terminal in its own OS/browser window,
//     seeded via the URL hash. It only loads /static/* assets and connects to
//     the /api/term-ws and /api/open-window-ws WebSocket endpoints.
//   - plain /terminals — the dashboard's own "Terminals" TAB under path routing
//     (TCL-317). Serve the SPA index so the URL /terminals and the visible tab
//     agree; the multiplexer lives in that tab (js/terminals-tab.js). Before
//     TCL-317 a plain /terminals also served the standalone page, but nothing
//     relies on that — the popout always carries ?solo=1.
func handleDashboardTerminals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// Auth: remote (mTLS + passphrase) requests are authenticated at the remote
	// listener's boundary; loopback requires the session cookie the browser got
	// when it opened the dashboard at /. A missing/stale cookie bounces to / to
	// re-authenticate rather than dead-ending on a plain 403.
	authed := dashboardPreAuthed(r)
	if !authed {
		if c, err := r.Cookie(dashboardCookieName); err == nil {
			valid, refresh := dashboardSessionCookieMatch(c.Value)
			authed = valid
			if refresh {
				setDashboardSessionCookie(w)
			}
		}
	}
	if !authed {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.URL.Query().Has("solo") {
		writeTerminalsPage(w)
		return
	}
	writeDashboardPage(w)
}

func writeTerminalsPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(terminalsPageHTML)
}

// writeDashboardPage writes the dashboard SPA index with the standard headers.
// Shared by the plain /terminals route so its tab restores like any other path.
func writeDashboardPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardIndexHTML)
}
