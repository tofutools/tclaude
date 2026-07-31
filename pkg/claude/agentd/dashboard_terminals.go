package agentd

import "net/http"

// terminalsPageHTML is terminals.html, read once at init — the standalone
// /terminals page handleDashboardTerminals serves.
var terminalsPageHTML = mustReadFS(dashboardAssetsFS, "terminals.html")

// handleDashboardTerminals serves the /terminals route, behind the same auth
// gate as the dashboard root. It has two jobs, discriminated by the ?solo query:
//
//   - /terminals?solo=1 — exactly that, value and all — the standalone popout
//     page (js/terminals.js): the
//     per-terminal "⧉ tab" pop-out, one terminal in its own OS/browser window,
//     seeded via the URL hash. It only loads /static/* assets and connects to
//     the /api/term-ws and /api/open-window-ws WebSocket endpoints.
//   - plain /terminals — the dashboard's own "Terminals" TAB under path routing
//     (TCL-317). Serve the SPA index so the URL /terminals and the visible tab
//     agree; the Preact terminal shell lives in that tab. Before
//     TCL-317 a plain /terminals also served the standalone page, but nothing
//     relies on that — the popout always carries ?solo=1.
func handleDashboardTerminals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	// Auth: remote (mTLS + passphrase) requests are authenticated at the remote
	// listener's boundary; loopback requires the session cookie the browser got
	// when it opened the dashboard at /.
	authed := dashboardPreAuthed(r)
	if !authed {
		_, valid, refresh := dashboardRequestSessionMatch(r)
		authed = valid
		if refresh {
			setDashboardSessionCookie(w)
		}
	}
	if !authed {
		// Render the sign-in page IN PLACE, exactly as handleDashboardRoot does
		// for a deep path, rather than bouncing to "/". The URL is what carries
		// the deep link, so a redirect would discard the agent before the
		// operator could sign in — losing the terminal on the one path a
		// bookmark is most likely to take (a stale cookie after a daemon
		// restart). Keeping it means dashboardLoginReturnTarget can restore
		// /terminals/<agent-id> afterwards, which is why it admits this route.
		renderDashboardLoginPage(w, r, http.StatusForbidden, "")
		return
	}
	// The pop-out is always the bare /terminals?solo=1. A deep link
	// (/terminals/<agent-id>) names a tab within the dashboard, so it serves the
	// SPA even if a stray ?solo rides along — the standalone page has no router
	// and would silently drop the agent segment.
	if r.URL.Path == "/terminals" && r.URL.Query().Get("solo") == "1" {
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
