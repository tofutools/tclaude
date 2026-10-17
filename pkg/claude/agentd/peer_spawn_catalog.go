package agentd

import (
	"context"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The spawn dialog's catalogs over the peer view (docs/federation.md, "The
// spawn dialog's catalogs"). A remote spawn opens this node's regular spawn
// dialog on the requesting node (agent-spawn-remote.js); node.spawn.read lets
// it read what this node's own dialog reads -- spawn profiles and the default,
// launch defaults, saved prompts, roles, sandbox profiles and defaults, spawn
// harness policies, the default model -- so it pre-fills exactly what this
// node would apply, and offers this node's profiles and prompts next to the
// requester's own. Unrestricted trust holds it. Read-only: the routes are GET
// patterns (plus the sandbox enforcement preview, which writes nothing), and
// every other method on these paths stays local-only.
const PermNodeSpawnRead = "node.spawn.read"

// peerSpawnCatalogRoutes are the node.spawn.read routes.
var peerSpawnCatalogRoutes = map[string]http.HandlerFunc{
	"GET /api/spawn-profiles":                     dashboardSpawnProfilesRoute(handleSpawnProfiles),
	"GET /api/spawn-profile-default":              dashboardSpawnProfilesRoute(handleGlobalDefaultSpawnProfile),
	"GET /api/spawn-launch-defaults":              dashboardSpawnProfilesRoute(handleSpawnLaunchDefaults),
	"GET /api/saved-prompts":                      dashboardRolesRoute(handleSavedPrompts),
	"GET /api/roles":                              dashboardRolesRoute(handleRoles),
	"GET /api/sandbox-profiles":                   dashboardSandboxProfilesRoute(handleSandboxProfiles),
	"GET /api/sandbox-profile-default":            dashboardSandboxProfilesRoute(handleGlobalSandboxProfile),
	"GET /api/groups/{group}/sandbox-profile":     dashboardSandboxProfilesRoute(handleGroupSandboxProfile),
	"POST /api/sandbox-profile-enforcement":       dashboardSandboxProfilesRoute(handleSandboxProfileDraftEnforcement),
	"GET /api/spawn-harness-policy":               handleDashboardGlobalSpawnHarnessPolicy,
	"GET /api/groups/{name}/spawn-harness-policy": groupRoute(handleDashboardGroupSpawnHarnessPolicy),
	"GET /api/claude-settings/default-model":      handleDashboardClaudeDefaultModel,
	"GET /api/spawn/effective-sandbox":            handleDashboardSpawnEffectiveSandbox,
}

// peerSpawnCatalogGroupParam names the path's group on the routes that read
// one group's settings: a peer reads them only for a group it can see.
var peerSpawnCatalogGroupParam = map[string]string{
	"GET /api/groups/{group}/sandbox-profile":     "group",
	"GET /api/groups/{name}/spawn-harness-policy": "name",
}

func addPeerSpawnCatalogRules(rules map[string]peerViewRule) {
	for pattern, next := range peerSpawnCatalogRoutes {
		rule := peerViewRule{feature: "spawn.catalog", requires: PermNodeSpawnRead}
		serve := servePeerSpawnCatalog(next, peerSpawnCatalogGroupParam[pattern])
		if strings.HasPrefix(pattern, "GET ") {
			rule.serve = serve
		} else {
			rule.write = serve
		}
		rules[pattern] = rule
	}
}

// servePeerSpawnCatalog runs this node's own dashboard read for a peer that
// holds node.spawn.read (and, on a group's route, can see that group).
func servePeerSpawnCatalog(next http.HandlerFunc, groupParam string) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !v.allows(rule, 0) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
			return
		}
		if groupParam != "" {
			g, err := db.GetAgentGroupByName(r.PathValue(groupParam))
			if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
				writeError(w, http.StatusNotFound, "not_found", "object not found")
				return
			}
		}
		next(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
	}
}

// peerSpawnCatalogReadable reports whether peer may read the catalogs.
func peerSpawnCatalogReadable(v *peerView) bool {
	return v.allows(peerViewRule{feature: "spawn.catalog", requires: PermNodeSpawnRead}, 0)
}
