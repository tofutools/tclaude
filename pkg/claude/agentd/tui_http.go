package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// buildTUIHTTPHandler exposes only the versioned operations the terminal
// dashboard consumes. It deliberately does not mount buildMux wholesale on a
// network listener: the operator token is powerful, but a narrow surface
// keeps an accidental dashboard bind from turning every agent-only endpoint
// into a remotely reachable control plane.
func buildTUIHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/peers", handlePeers)
	mux.HandleFunc("GET /v1/groups", handleGroups)
	mux.HandleFunc("GET /v1/spawn-profiles", handleSpawnProfiles)
	mux.HandleFunc("POST /v1/groups/{name}/spawn", v1GroupRoute(handleGroupSpawn))
	mux.HandleFunc("POST /v1/agent/{selector}/retire", handleAgentByConv)
	mux.HandleFunc("POST /v1/agent/{selector}/resume", handleAgentByConv)

	// Preserve the same mutation idempotency, request logging, and audit path
	// the Unix-socket /v1 mux applies. The prefix has already been stripped,
	// so audit sees the canonical /v1 operation.
	api := idempotencyRequests(logRequest(auditRequests(mux)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticateTUIHTTPRequest(w, r) {
			return
		}
		api.ServeHTTP(w, asDashboardHumanPeer(r))
	})
}

// authenticateTUIHTTPRequest accepts either an existing dashboard session or
// the ordinary operator-token header used by CLI requests. A valid token also
// mints the dashboard cookie into the HTTP client's jar. That gives the
// standalone TUI the web dashboard's clean-restart handoff: the next daemon
// accepts the grace cookie and rotates it to the new process's session.
//
// A browser cannot forge this bootstrap cross-origin: the custom token header
// triggers CORS preflight, and these routes grant no cross-origin policy.
func authenticateTUIHTTPRequest(w http.ResponseWriter, r *http.Request) bool {
	if dashboardPreAuthed(r) {
		return true
	}
	if operatorTokenMatches(r.Header.Get(agent.HumanTokenHeader)) {
		if dashboardSessionToken == "" {
			http.Error(w, "dashboard not initialised", http.StatusServiceUnavailable)
			return false
		}
		setDashboardSessionCookie(w)
		return true
	}
	// The standalone client may address a loopback listener by localhost
	// while popupBaseURL names 127.0.0.1 (or sit behind a host-rewriting
	// reverse proxy). Pinning to popupBaseURL would reject the grace cookie
	// precisely after a restart rotates the operator token. The client always
	// sends Origin derived from its target, so require host-relative
	// same-origin here, as the non-loopback dashboard path does.
	present, valid, refresh := dashboardRequestSessionMatch(r)
	if !present || !valid {
		w.Header().Set("X-Tclaude-Login-Required", "1")
		http.Error(w, "missing or invalid dashboard cookie", http.StatusForbidden)
		return false
	}
	if !dashboardHostRelativeOrigin(r) {
		http.Error(w, "Origin/Referer host mismatch", http.StatusForbidden)
		return false
	}
	if refresh {
		setDashboardSessionCookie(w)
	}
	return true
}
