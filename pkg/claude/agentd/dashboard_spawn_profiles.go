package agentd

import "net/http"

// Cookie-auth /api/spawn-profiles endpoints — the loopback twins of the
// daemon's /v1/spawn-profiles surface (SO_PEERCRED-authed on the Unix socket,
// which the browser can't speak). Each route delegates to the SHARED,
// permission-checked /v1 handler after stamping a synthetic human peer with
// asDashboardHumanPeer, so the profiles.manage slug check stays structurally
// enforced on every path. Same wiring as the /api/templates routes.

// registerDashboardSpawnProfileRoutes wires the cookie-authed
// /api/spawn-profiles endpoints onto the loopback mux:
//
//	GET    /api/spawn-profiles              → list profiles
//	POST   /api/spawn-profiles              → create a profile
//	POST   /api/spawn-profiles/from-agent   → capture a live agent's config into an unsaved seed
//	GET    /api/spawn-profiles/export       → export selected/all profiles as a portable bundle
//	POST   /api/spawn-profiles/import/inspect → preview a portable profile bundle
//	POST   /api/spawn-profiles/import       → import a portable profile bundle
//	GET    /api/spawn-profiles/{name}       → fetch one profile
//	PATCH  /api/spawn-profiles/{name}       → replace a profile
//	DELETE /api/spawn-profiles/{name}       → delete a profile
//	GET/PUT/DELETE /api/spawn-profile-default → manage the global default
func registerDashboardSpawnProfileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/spawn-profiles", dashboardSpawnProfilesRoute(handleSpawnProfiles))
	mux.HandleFunc("/api/spawn-profile-default", dashboardSpawnProfilesRoute(handleGlobalDefaultSpawnProfile))
	// Literal segments are more specific than {name}, so the mux picks them
	// unambiguously (mirrors /api/templates/from-group/import/export).
	mux.HandleFunc("POST /api/spawn-profiles/from-agent", dashboardSpawnProfilesRoute(handleSpawnProfileFromAgent))
	mux.HandleFunc("GET /api/spawn-profiles/export", dashboardSpawnProfilesRoute(handleSpawnProfilesExport))
	mux.HandleFunc("POST /api/spawn-profiles/import/inspect", dashboardSpawnProfilesRoute(handleSpawnProfilesImportInspect))
	mux.HandleFunc("POST /api/spawn-profiles/import", dashboardSpawnProfilesRoute(handleSpawnProfilesImport))
	mux.HandleFunc("/api/spawn-profiles/{name}", dashboardSpawnProfilesRoute(handleSpawnProfileByName))
}

// dashboardSpawnProfilesRoute adapts a shared /v1 spawn-profile handler into a
// cookie-authed /api handler: it runs the dashboard cookie/Origin auth, then
// hands the handler a synthetic human peer so the inner requirePermission
// treats the cookie-authed dashboard caller as the human. The {name} path
// wildcard survives asDashboardHumanPeer, so r.PathValue("name") still
// resolves.
func dashboardSpawnProfilesRoute(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		fn(w, asDashboardHumanPeer(r))
	}
}
