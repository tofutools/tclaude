package agentd

import (
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func hostPublishAllowed(convID string) bool {
	src, err := loadPermSourcesWithReadPolicy(convID, true)
	if err != nil {
		return false
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	verdict := resolveEffectivePermissionVerdictFrom(localPermissionSources(src), PermRoutesPublishHost, cfg.HasDefaultPermission(PermRoutesPublishHost), cfg.HasDefaultPermission(PermGroupsAdmin))
	return verdict.Resolution == permAllow && evalPermissionScope(verdict, convID, ActionContext{}).Satisfied
}

func handleHostOnlyRoutesList(w http.ResponseWriter, r *http.Request) {
	publisher := ""
	if classify(peerFromContext(r.Context())) != classHuman {
		_, agentID, ok := routeCallerAgent(w, r)
		if !ok {
			return
		}
		publisher = agentID
	}
	routes, err := db.ListHostAgentRoutes(publisher)
	if err != nil {
		writeRouteError(w, 500, "route_io", "could not list host routes")
		return
	}
	views := []routeView{}
	for _, route := range routes {
		views = append(views, routeViewFor(refreshRoutePublisher(route)))
	}
	writeJSON(w, 200, map[string]any{"api_version": routeAPIVersion, "routes": views})
}

func handleHostOnlyRoute(w http.ResponseWriter, r *http.Request, route *db.AgentRoute) {
	if classify(peerFromContext(r.Context())) != classHuman {
		_, agentID, ok := routeCallerAgent(w, r)
		if !ok {
			return
		}
		if agentID != route.PublisherAgentID {
			writeRouteError(w, 404, "route_not_found", "no such route")
			return
		}
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, routeViewFor(route))
		return
	}
	if r.Method != http.MethodDelete && !(r.Method == http.MethodPost && strings.Trim(r.PathValue("action"), "/") == "withdraw") {
		writeRouteError(w, 403, "route_host_only", "host-only routes cannot be consumed by agents")
		return
	}
	convID, agentID, ok := routeCallerAgent(w, r)
	if !ok {
		return
	}
	if agentID != route.PublisherAgentID {
		writeRouteError(w, 403, "route_not_owner", "only the publisher may withdraw this route")
		return
	}
	if _, ok := requirePermission(w, r, PermRoutesPublishHost); !ok {
		return
	}
	if err := db.WithdrawAgentRoute(route.ID, agentID, convID, "publisher requested withdrawal"); err != nil {
		writeRouteError(w, 409, "route_transition", err.Error())
		return
	}
	routeAdapterCloseRoute(route.ID)
	writeJSON(w, 200, routeViewFor(mustRoute(route.ID)))
}
