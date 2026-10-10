package agentd

import "net/http"

func registerDashboardFederationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/federation/offer-config", dashboardFederationRoute(handleFederationOfferConfig))
	mux.HandleFunc("POST /api/federation/share-agent", dashboardFederationRoute(handleFederationShareAgent))
	mux.HandleFunc("GET /api/federation/bundle-offers", dashboardFederationRoute(handleFederationBundleOffers))
	mux.HandleFunc("POST /api/federation/bundle-offers/{id}/fetch", dashboardFederationRoute(handleFederationBundleFetch))
	mux.HandleFunc("POST /api/federation/bundle-offers/{id}/import", dashboardFederationRoute(handleFederationBundleImport))
	mux.HandleFunc("POST /api/federation/bundle-offers/{id}/decline", dashboardFederationRoute(handleFederationBundleDecline))
	mux.HandleFunc("POST /api/federation/profiles/{name}/offer", dashboardFederationRoute(handleFederationNodeProfileOffer))
	mux.HandleFunc("POST /api/federation/send", dashboardFederationRoute(handleFederationSend))
	mux.HandleFunc("POST /api/federation/notify", dashboardFederationRoute(handleFederationNotify))
	mux.HandleFunc("GET /api/federation/inbox", dashboardFederationRoute(handleFederationInbox))
	mux.HandleFunc("GET /api/federation/away", dashboardFederationRoute(handleFederationAway))
	mux.HandleFunc("POST /api/federation/away", dashboardFederationRoute(handleFederationAway))
	mux.HandleFunc("POST /api/federation/return", dashboardFederationRoute(handleFederationReturn))
	mux.HandleFunc("POST /api/federation/answer", dashboardFederationRoute(handleFederationAwayAnswer))
	mux.HandleFunc("GET /api/federation/moves", dashboardFederationRoute(handleFederationMoves))
	mux.HandleFunc("GET /api/federation/moves/{id}", dashboardFederationRoute(handleFederationMoves))
	mux.HandleFunc("POST /api/federation/moves/{id}/abandon", dashboardFederationRoute(handleFederationMoveAbandon))
	mux.HandleFunc("GET /api/federation/teleport", dashboardFederationRoute(handleFederationTeleportSwitch))
	mux.HandleFunc("PUT /api/federation/teleport", dashboardFederationRoute(handleFederationTeleportSwitch))
	mux.HandleFunc("GET /api/federation/access-requests", dashboardFederationRoute(handleFederationAccessRequests))
	mux.HandleFunc("POST /api/federation/access-requests/{id}/decision", dashboardFederationRoute(handleFederationAccessRequests))
	mux.HandleFunc("GET /api/federation/status", dashboardFederationRoute(handleFederationStatus))
	mux.HandleFunc("GET /api/federation/links", dashboardFederationRoute(handleFederationLinks))
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
