package agentd

import (
	"errors"
	"fmt"
	"log/slog"
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
	canWithdraw := false
	switch r.Method {
	case http.MethodDelete:
		canWithdraw = true
	case http.MethodPost:
		canWithdraw = strings.Trim(r.PathValue("action"), "/") == "withdraw"
	}
	if !canWithdraw {
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
	return hostRouteHelperIneligibleReason(harnessName, implementation, snapshot) == ""
}

// hostRouteHelperIneligibleReason explains why a launch cannot carry the
// host-route helper, or returns "" when it can. The wording completes the
// sentence "host routes are unavailable because …".
func hostRouteHelperIneligibleReason(harnessName, implementation string, snapshot *sandboxpolicy.Snapshot) string {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "host routes are only supported on Linux and macOS"
	}
	if implementation != string(sandboxpolicy.ImplementationTclaudeLayer) {
		return "the agent is not launched in tclaude’s pane sandbox"
	}
	if session.TclaudeLayerUsesServerBoundary(harnessName) {
		return fmt.Sprintf("the %s harness runs behind a server boundary with no pane-local route helper", harnessName)
	}
	if snapshot == nil {
		return "the launch has no resolved sandbox profile"
	}
	if reason := hostRouteNetworkReason(snapshot); reason != "" {
		return reason
	}
	return ""
}

func hostRouteNetworkReason(snapshot *sandboxpolicy.Snapshot) string {
	posture, err := session.TclaudeLayerNetworkPosture(snapshot.Effective)
	if err != nil {
		return "the sandbox network posture could not be resolved: " + err.Error()
	}
	if posture == sandboxpolicy.NetworkHostOpen {
		return "the agent’s sandbox profile uses open host networking; host routes need a private (filtered or isolated) network"
	}
	return ""
}

// warnHostRouteHelperSkipped logs when an agent holding routes.publish-host
// is launched without the host-route helper, so the later publish failure is
// not the operator's first sign of it.
func warnHostRouteHelperSkipped(convID, surface, reason string) {
	slog.Warn("agent holds routes.publish-host but this launch has no host-route helper; host publishing will fail until it is resumed in an eligible launch",
		"conv", convID, "surface", surface, "reason", reason)
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
	return errors.New("host publishing requires a route-capable launch, but " + hostPublishLaunchDiagnosis(agentID, convID, generation) + ". " + hostPublishLaunchRemedy)
}

const hostPublishLaunchRemedy = "Host routes need tclaude’s pane sandbox with a private network, a pane-local harness, and the grant present when the agent launches or resumes; once those hold, stop the agent and run `tclaude agent resume` on it"

// hostPublishLaunchDiagnosis names the most specific known reason the current
// launch holds no host-route helper. It reads only durable launch facts and
// the daemon's in-memory credential table; it never reports a credential.
func hostPublishLaunchDiagnosis(agentID, convID, generation string) string {
	if runtime.GOOS == "linux" {
		switch routeHelperCredentialStaleness(agentID, convID, generation) {
		case "other":
			return "this agent’s route helper belongs to a different launch generation than the one making this request"
		case "ended":
			return "this agent’s route helper belongs to a launch that is no longer live"
		}
	}
	h := harnessForConv(convID)
	harnessName := ""
	if h != nil {
		harnessName = h.Name
		if session.TclaudeLayerUsesServerBoundary(harnessName) {
			return fmt.Sprintf("the %s harness runs behind a server boundary with no pane-local route helper", harnessName)
		}
	}
	if snapshot, err := db.AgentEffectiveSandboxConfigForConv(convID); err == nil && snapshot != nil {
		if reason := hostRouteNetworkReason(snapshot); reason != "" {
			return reason
		}
	}
	// Linux helper capabilities live only in daemon memory; macOS route
	// launches are durable records that survive an agentd restart.
	restart := ""
	if runtime.GOOS == "linux" {
		restart = ", and an agentd restart since then invalidates it again"
	}
	if h != nil && !h.SupportsLaunchEnrollment() {
		return fmt.Sprintf("no route helper is active for this launch: a fresh %s spawn never gets one, because its conversation id is unknown before the pane starts, so the helper is only attached on a later resume%s", harnessName, restart)
	}
	if runtime.GOOS == "linux" {
		return "no route helper is active for this launch (the grant may have been added after launch, the launch may not use tclaude’s pane sandbox, or agentd restarted since, which invalidates every helper)"
	}
	return "no route-capable launch is active (the grant may have been added after launch, or the launch may not use tclaude’s pane sandbox)"
}
