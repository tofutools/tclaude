package agentd

import "net/http"

// registerDashboardUsageRuleRoutes serves the usage-rules API to the
// dashboard as the synthetic human peer. The peer view classifies each route
// in addPeerUsageRules.
func registerDashboardUsageRuleRoutes(mux *http.ServeMux) {
	wrap := dashboardSandboxProfilesRoute // dashboard auth + synthetic human peer
	mux.HandleFunc("GET /api/usage/rules", wrap(handleUsageRulesOverview))
	mux.HandleFunc("POST /api/usage/profiles", wrap(handleUsageProfileCreate))
	mux.HandleFunc("PUT /api/usage/profiles/{ref}", wrap(handleUsageProfileUpdate))
	mux.HandleFunc("DELETE /api/usage/profiles/{ref}", wrap(handleUsageProfileDelete))
	mux.HandleFunc("POST /api/usage/limits", wrap(handleUsageLimitCreate))
	mux.HandleFunc("PUT /api/usage/limits/{id}", wrap(handleUsageLimitUpdate))
	mux.HandleFunc("DELETE /api/usage/limits/{id}", wrap(handleUsageLimitDelete))
	mux.HandleFunc("PUT /api/usage/assignments", wrap(handleUsageAssignmentsSet))
	mux.HandleFunc("POST /api/usage/agents/{selector}/adjust", wrap(handleUsageAgentAdjust))
	mux.HandleFunc("GET /api/usage/node-summary", wrap(handleUsageSummary))
	mux.HandleFunc("GET /api/usage/fleet", wrap(handleUsageFleet))
	mux.HandleFunc("POST /api/usage/fleet-limits", wrap(handleUsageFleetLimitCreate))
	mux.HandleFunc("PUT /api/usage/fleet-limits/{id}", wrap(handleUsageFleetLimitUpdate))
	mux.HandleFunc("DELETE /api/usage/fleet-limits/{id}", wrap(handleUsageFleetLimitDelete))
}
