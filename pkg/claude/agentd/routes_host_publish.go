package agentd

import (
	"errors"
	"net/http"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	if !(r.Method == http.MethodDelete || r.Method == http.MethodPost && strings.Trim(r.PathValue("action"), "/") == "withdraw") {
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

func hostRouteHelperEligible(harnessName, implementation string, snapshot *sandboxpolicy.Snapshot) bool {
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || implementation != string(sandboxpolicy.ImplementationTclaudeLayer) || session.TclaudeLayerUsesServerBoundary(harnessName) || snapshot == nil {
		return false
	}
	posture, err := session.TclaudeLayerNetworkPosture(snapshot.Effective)
	return err == nil && posture != sandboxpolicy.NetworkHostOpen
}

func validateHostPublishLaunch(agentID, convID, generation string) error {
	if runtime.GOOS == "linux" && routeHelperCapabilityActive(agentID, convID, generation) {
		return nil
	}
	if runtime.GOOS == "darwin" {
		launch, err := db.GetDarwinRouteLaunch(agentID, convID, generation)
		if err == nil && launch != nil && launch.State == db.DarwinRouteLaunchActive {
			return nil
		}
	}
	return errors.New("host publishing requires a current route-capable launch: grant routes.publish-host, then relaunch with tclaude’s pane sandbox and a private network; seed-based harnesses may need an initial launch followed by resume")
}
