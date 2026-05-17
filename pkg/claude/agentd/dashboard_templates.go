package agentd

import "net/http"

// Cookie-auth /api/templates endpoints — the loopback twins of the
// daemon's /v1/templates surface (SO_PEERCRED-authed on the Unix
// socket, which the browser can't speak).
//
// Each route delegates to the SHARED, permission-checked /v1 handler
// after stamping a synthetic human peer with asDashboardHumanPeer: the
// dashboard cookie + Origin pin is the human-consent layer, and routing
// through the shared handler keeps the templates.* slug checks
// structurally enforced on every path (requirePermission sees a
// classHuman caller — asDashboardHumanPeer sets DashboardHuman). Same
// wiring as the /api/groups routes in dashboard_edit.go.

// registerDashboardTemplateRoutes wires the cookie-authed /api/templates
// endpoints onto the loopback mux:
//
//	GET    /api/templates                       → list templates
//	POST   /api/templates                       → create a template
//	GET    /api/templates/{name}                → fetch one template
//	PATCH  /api/templates/{name}                → replace a template
//	DELETE /api/templates/{name}                → delete a template
//	POST   /api/templates/{name}/instantiate    → create a group + spawn its team
//	POST   /api/templates/from-group            → snapshot a live group into a template
//
// The literal `from-group` / `instantiate` segments are more specific
// than the {name} wildcard, so the mux picks them unambiguously.
func registerDashboardTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/templates", dashboardTemplatesRoute(handleTemplates))
	mux.HandleFunc("POST /api/templates/from-group", dashboardTemplatesRoute(handleTemplateFromGroup))
	mux.HandleFunc("POST /api/templates/{name}/instantiate", dashboardTemplatesRoute(handleTemplateInstantiate))
	mux.HandleFunc("/api/templates/{name}", dashboardTemplatesRoute(handleTemplateByName))
}

// dashboardTemplatesRoute adapts a shared /v1 template handler into a
// cookie-authed /api handler: it runs the dashboard cookie/Origin auth,
// then hands the handler a synthetic human peer so the inner
// requirePermission treats the cookie-authed dashboard caller as the
// human. The {name} path wildcard survives asDashboardHumanPeer (it
// only swaps the request context), so the shared handler's
// r.PathValue("name") still resolves.
func dashboardTemplatesRoute(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		fn(w, asDashboardHumanPeer(r))
	}
}
