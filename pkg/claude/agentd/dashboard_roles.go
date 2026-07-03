package agentd

import "net/http"

// Cookie-auth /api/roles endpoints — the loopback twins of the daemon's
// /v1/roles surface (SO_PEERCRED-authed on the Unix socket, which the browser
// can't speak). Each route delegates to the SHARED, permission-checked /v1
// handler after stamping a synthetic human peer with asDashboardHumanPeer, so
// the roles.manage slug check stays structurally enforced on every path. Same
// wiring as the /api/spawn-profiles routes.

// registerDashboardRoleRoutes wires the cookie-authed /api/roles endpoints onto
// the loopback mux:
//
//	GET    /api/roles          → list roles
//	POST   /api/roles          → create a role
//	GET    /api/roles/{name}   → fetch one role
//	PATCH  /api/roles/{name}   → replace a role
//	DELETE /api/roles/{name}   → delete a role
func registerDashboardRoleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/roles", dashboardRolesRoute(handleRoles))
	mux.HandleFunc("/api/roles/{name}", dashboardRolesRoute(handleRoleByName))
}

// dashboardRolesRoute adapts a shared /v1 role handler into a cookie-authed
// /api handler: it runs the dashboard cookie/Origin auth, then hands the
// handler a synthetic human peer so the inner requirePermission treats the
// cookie-authed dashboard caller as the human. The {name} path wildcard
// survives asDashboardHumanPeer, so r.PathValue("name") still resolves.
func dashboardRolesRoute(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		fn(w, asDashboardHumanPeer(r))
	}
}
