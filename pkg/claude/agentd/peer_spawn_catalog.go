package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The spawn dialog's catalogs over the peer view (docs/federation.md, "The
// spawn dialog's catalogs"). A remote spawn opens this node's regular spawn
// dialog on the requesting node (agent-spawn-remote.js); node.spawn.read lets
// it read what this node's own dialog reads -- spawn profiles and the default,
// launch defaults, saved prompts, roles, sandbox profiles and defaults, spawn
// harness policies, the default model, the harness and sandbox
// implementation catalogs -- so it pre-fills exactly what this
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
	"GET /api/spawn-launch-catalog":               handleDashboardSpawnLaunchCatalog,
}

// peerSpawnCatalogGroupParam names the path's group on the routes that read
// one group's settings: a peer reads them only for a group it can see.
var peerSpawnCatalogGroupParam = map[string]string{
	"GET /api/groups/{group}/sandbox-profile":     "group",
	"GET /api/groups/{name}/spawn-harness-policy": "name",
}

// peerSpawnCatalogGuards narrow what a route's request may name on the peer
// path, beyond its group path parameter: a group in a query or body only when
// the peer can see it, a directory only one the peer could launch in anyway,
// and never another stored profile's identity.
var peerSpawnCatalogGuards = map[string]func(*http.Request, *peerView) bool{
	"GET /api/spawn-launch-defaults":        peerCatalogQueryGroupVisible,
	"GET /api/spawn/effective-sandbox":      peerCatalogEffectiveSandboxDir,
	"POST /api/sandbox-profile-enforcement": peerCatalogEnforcementDraft,
}

func addPeerSpawnCatalogRules(rules map[string]peerViewRule) {
	for pattern, next := range peerSpawnCatalogRoutes {
		rule := peerViewRule{feature: "spawn.catalog", requires: PermNodeSpawnRead}
		serve := servePeerSpawnCatalog(next, peerSpawnCatalogGroupParam[pattern], peerSpawnCatalogGuards[pattern])
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
func servePeerSpawnCatalog(next http.HandlerFunc, groupParam string, guard func(*http.Request, *peerView) bool) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !v.allows(rule, 0) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
			return
		}
		if groupParam != "" && !peerCatalogGroupVisible(r.PathValue(groupParam), v) || guard != nil && !guard(r, v) {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		next(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
	}
}

func peerCatalogGroupVisible(name string, v *peerView) bool {
	g, err := db.GetAgentGroupByName(name)
	return err == nil && g != nil && !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID)
}

// peerCatalogQueryGroupVisible: launch defaults resolve a ?group= group's
// default profile.
func peerCatalogQueryGroupVisible(r *http.Request, v *peerView) bool {
	group := r.URL.Query().Get("group")
	return group == "" || peerCatalogGroupVisible(group, v)
}

// peerCatalogEffectiveSandboxDir: the effective-sandbox read walks project
// settings up from ?dir=, which would let a peer probe this host's
// filesystem. A peer may name a directory it could launch in anyway: any,
// for unrestricted trust; otherwise a visible group's default directory or
// its spawn grant's pinned one.
func peerCatalogEffectiveSandboxDir(r *http.Request, v *peerView) bool {
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dir == "" || db.FederationPeerUnrestricted(v.peer.InstanceID) {
		return true
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			continue
		}
		if g.DefaultCwd == dir {
			return true
		}
		if grant := fedPeerGroupGrant(v.peer.InstanceID, g.ID, PermGroupsMembersSpawn); grant != nil && grant.SpawnPolicy.Cwd == dir {
			return true
		}
	}
	return false
}

// peerCatalogEnforcementDraft: the enforcement preview of a draft names
// every group using the stored profile the draft's id points at, and the
// context group's policy. A peer previews a draft as a new profile, in a
// group it can see.
func peerCatalogEnforcementDraft(r *http.Request, v *peerView) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	var draft map[string]json.RawMessage
	if body["draft"] != nil {
		if json.Unmarshal(body["draft"], &draft) != nil {
			return false
		}
		delete(draft, "id")
		body["draft"], _ = json.Marshal(draft)
	}
	if body["context"] != nil {
		var hint struct {
			Group string `json:"group"`
		}
		if json.Unmarshal(body["context"], &hint) != nil || hint.Group != "" && !peerCatalogGroupVisible(hint.Group, v) {
			return false
		}
	}
	raw, _ = json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return true
}

// peerSpawnCatalogReadable reports whether peer may read the catalogs.
func peerSpawnCatalogReadable(v *peerView) bool {
	return v.allows(peerViewRule{feature: "spawn.catalog", requires: PermNodeSpawnRead}, 0)
}

// handleDashboardSpawnLaunchCatalog serves the launch catalogs the spawn
// dialog reads from the dashboard snapshot -- the spawnable harnesses with
// their models and modes, and the sandbox implementations -- on their own, so
// a peer's dialog (which the peer-view snapshot does not give them) offers
// what this node can launch.
func handleDashboardSpawnLaunchCatalog(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"harnesses":    buildHarnessCatalog(),
		"sandbox_impl": buildSandboxImplCatalog(),
	})
}
