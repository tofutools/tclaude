package agentd

import "net/http"

// The Config page's management of external stewards: the human-only /v1 handlers
// behind the dashboard's own auth. A viewing peer never reaches them. The list is
// this node's, and the responses that issue a credential must stay in this
// browser, so these are refused for forwarded and peer-granted requests too.
func registerDashboardExternalStewardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/external-stewards", dashboardExternalStewardRoute(handleExternalStewardsList))
	mux.HandleFunc("POST /api/external-stewards", dashboardExternalStewardRoute(handleExternalStewardCreate))
	mux.HandleFunc("POST /api/external-stewards/{name}/renew", dashboardExternalStewardRoute(handleExternalStewardRenew))
	mux.HandleFunc("POST /api/external-stewards/{name}/revoke", dashboardExternalStewardRoute(handleExternalStewardRevoke))
	mux.HandleFunc("POST /api/external-stewards/{name}/spawn", dashboardExternalStewardRoute(handleExternalStewardSpawnPin))
}

func dashboardExternalStewardRoute(next http.HandlerFunc) http.HandlerFunc {
	return collabRoute(func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		if peerActionFromRequest(r) != nil || peerGrantFromRequest(r) != nil {
			writeError(w, http.StatusForbidden, "auth", "external stewards are managed on their own instance")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, asDashboardHumanPeer(r))
	})
}
