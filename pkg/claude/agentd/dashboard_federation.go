package agentd

import "net/http"

func registerDashboardFederationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/federation/status", dashboardFederationRoute(handleFederationStatus))
}

// Federation administration belongs to the cookie-authenticated local human.
// PeerViewHandler classifies these routes as local-only and never calls this.
func dashboardFederationRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		next(w, asDashboardHumanPeer(r))
	}
}
