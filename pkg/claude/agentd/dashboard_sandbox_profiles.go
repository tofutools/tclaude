package agentd

import "net/http"

// registerDashboardSandboxProfileRoutes exposes the shared sandbox-profile
// handlers to the cookie-authenticated dashboard. The browser cannot use the
// Unix-socket /v1 routes directly, so each loopback route authenticates the
// dashboard request and stamps the same synthetic human peer used by the
// spawn-profile manager. Keeping the shared handler in the path preserves the
// sandbox-profiles.manage permission check on every payload read and mutation.
func registerDashboardSandboxProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/sandbox-profiles", dashboardSandboxProfilesRoute(handleSandboxProfiles))
	mux.HandleFunc("/api/sandbox-profile-default", dashboardSandboxProfilesRoute(handleGlobalSandboxProfile))
	mux.HandleFunc("GET /api/sandbox-profiles/export", dashboardSandboxProfilesRoute(handleSandboxProfilesExport))
	mux.HandleFunc("POST /api/sandbox-profiles/import", dashboardSandboxProfilesRoute(handleSandboxProfilesImport))
	mux.HandleFunc("/api/sandbox-profiles/{name}", dashboardSandboxProfilesRoute(handleSandboxProfileByName))
	mux.HandleFunc("/api/groups/{group}/sandbox-profile", dashboardSandboxProfilesRoute(handleGroupSandboxProfile))
}

func dashboardSandboxProfilesRoute(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		fn(w, asDashboardHumanPeer(r))
	}
}
