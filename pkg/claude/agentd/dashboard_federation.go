package agentd

import "net/http"

func registerDashboardFederationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/federation/status", dashboardFederationRoute(handleFederationStatus))
	mux.HandleFunc("GET /api/federation/audit", dashboardFederationRoute(handleFederationAudit))
	mux.HandleFunc("POST /api/federation/config", dashboardFederationRoute(handleFederationConfig))
	mux.HandleFunc("GET /api/federation/enroll-tokens", dashboardFederationRoute(handleFederationEnrollmentTokens))
	mux.HandleFunc("POST /api/federation/enroll-tokens", dashboardFederationRoute(handleFederationEnrollmentTokens))
	mux.HandleFunc("POST /api/federation/enroll-tokens/{id}/revoke", dashboardFederationRoute(handleFederationEnrollmentTokens))
	mux.HandleFunc("GET /api/federation/enrollments", dashboardFederationRoute(handleFederationEnrollments))
	mux.HandleFunc("POST /api/federation/enroll/preview", dashboardFederationRoute(handleFederationEnroll))
	mux.HandleFunc("POST /api/federation/enroll", dashboardFederationRoute(handleFederationEnroll))
	mux.HandleFunc("POST /api/federation/peers/trust", dashboardFederationRoute(handleFederationTrust))
	mux.HandleFunc("POST /api/federation/peers/untrust", dashboardFederationRoute(handleFederationUntrust))
	mux.HandleFunc("GET /api/federation/grants", dashboardFederationRoute(handleFederationPeerGrants))
	mux.HandleFunc("POST /api/federation/grants", dashboardFederationRoute(handleFederationPeerGrants))
	mux.HandleFunc("DELETE /api/federation/grants", dashboardFederationRoute(handleFederationPeerGrants))
	mux.HandleFunc("GET /api/federation/profiles", dashboardFederationRoute(handleFederationNodeProfiles))
	mux.HandleFunc("POST /api/federation/profiles", dashboardFederationRoute(handleFederationNodeProfiles))
	mux.HandleFunc("GET /api/federation/profiles/{name}", dashboardFederationRoute(handleFederationNodeProfiles))
	mux.HandleFunc("PUT /api/federation/profiles/{name}", dashboardFederationRoute(handleFederationNodeProfiles))
	mux.HandleFunc("DELETE /api/federation/profiles/{name}", dashboardFederationRoute(handleFederationNodeProfiles))
	mux.HandleFunc("POST /api/federation/profiles/{name}/apply", dashboardFederationRoute(handleFederationNodeProfileApply))
	mux.HandleFunc("PUT /api/federation/default-peer-profile", dashboardFederationRoute(handleFederationDefaultPeerProfile))
	mux.HandleFunc("GET /api/federation/nodes/groups", dashboardFederationRoute(handleFederationNodeGroups))
	mux.HandleFunc("POST /api/federation/nodes/groups", dashboardFederationRoute(handleFederationNodeGroups))
	mux.HandleFunc("DELETE /api/federation/nodes/groups/{name}", dashboardFederationRoute(handleFederationNodeGroups))
	mux.HandleFunc("POST /api/federation/nodes/groups/{name}/members", dashboardFederationRoute(handleFederationNodeGroupMembers))
	mux.HandleFunc("DELETE /api/federation/nodes/groups/{name}/members", dashboardFederationRoute(handleFederationNodeGroupMembers))
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
