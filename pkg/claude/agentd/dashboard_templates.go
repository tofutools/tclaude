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
//	POST   /api/templates/{name}/deploy         → deploy a task force against a mission
//	POST   /api/templates/{name}/reinforce      → deploy the roster INTO an existing group
//	POST   /api/templates/from-group            → snapshot a live group into a template
//	GET    /api/templates/{name}/export         → download a portable template envelope
//	POST   /api/templates/import                → import a portable template envelope
//
// The literal `from-group` / `instantiate` / `deploy` segments are more
// specific than the {name} wildcard, so the mux picks them unambiguously.
func registerDashboardTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/templates", dashboardTemplatesRoute(handleTemplates))
	mux.HandleFunc("POST /api/templates/from-group", dashboardTemplatesRoute(handleTemplateFromGroup))
	mux.HandleFunc("POST /api/templates/import", dashboardTemplatesRoute(handleTemplateImport))
	mux.HandleFunc("POST /api/templates/{name}/instantiate", dashboardTemplatesRoute(handleTemplateInstantiate))
	mux.HandleFunc("POST /api/templates/{name}/deploy", dashboardTemplatesRoute(handleTemplateDeploy))
	mux.HandleFunc("POST /api/templates/{name}/reinforce", dashboardTemplatesRoute(handleTemplateReinforce))
	mux.HandleFunc("GET /api/templates/{name}/export", dashboardTemplatesRoute(handleTemplateExport))
	mux.HandleFunc("/api/templates/{name}", dashboardTemplatesRoute(handleTemplateByName))
	// Bundled starter task forces (JOH-246) — loopback twins of /v1/starters,
	// so the dashboard's templates overlay can offer "install a starter" too.
	mux.HandleFunc("GET /api/starters", dashboardTemplatesRoute(handleStarters))
	mux.HandleFunc("GET /api/starters/{name}", dashboardTemplatesRoute(handleStarterByName))
	mux.HandleFunc("POST /api/starters/{name}/install", dashboardTemplatesRoute(handleStarterInstall))
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
