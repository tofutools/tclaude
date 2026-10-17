package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The spawn dialog over the peer view (docs/federation.md, "The full spawn
// dialog from a peer"). Two grants lend it, and unrestricted trust holds both:
//
//   - node.spawn.read: the catalogs the dialog reads (spawn profiles and the
//     default, launch defaults, saved prompts, roles, sandbox profiles and
//     defaults, spawn harness policies, the default model) -- GET only, plus
//     the sandbox-profile enforcement preview, which writes nothing.
//   - groups.members.spawn.full (group-scoped): the dialog's own spawn, with
//     every field this node's operator has, run by this node's dashboard
//     spawn handler; and, with it in any group, the cwd pickers (directory
//     browser, worktree list, create and progress) that feed it.
//
// groups.members.spawn alone keeps the brief-only spawn request. Uploads
// (spawn attachments) stay local: they do not fit the peer view's request
// body limit.
const (
	PermNodeSpawnRead          = "node.spawn.read"
	PermGroupsMembersSpawnFull = "groups.members.spawn.full"
)

// peerSpawnCatalogRoutes are the node.spawn.read routes. Method-specific
// patterns: every other method on these paths stays local-only.
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

// peerSpawnPickerRoutes are the cwd pickers of a full spawn: allowed while the
// peer holds groups.members.spawn.full in any group.
var peerSpawnPickerRoutes = map[string]http.HandlerFunc{
	"POST /api/browse-directories": handleDashboardBrowseDirsAPI,
	"GET /api/worktrees":           handleDashboardWorktreesAPI,
	"POST /api/worktrees":          handleDashboardWorktreesAPI,
	"GET /api/worktrees/progress":  dashboardWorktreeProgressAPI,
}

func addPeerSpawnDialogRules(rules map[string]peerViewRule) {
	for pattern, next := range peerSpawnCatalogRoutes {
		rule := peerViewRule{feature: "spawn.catalog", requires: PermNodeSpawnRead}
		serve := servePeerSpawnDialog(next, func(v *peerView, rule peerViewRule) bool { return v.allows(rule, 0) })
		if pattern[:4] == "GET " {
			rule.serve = serve
		} else {
			rule.write = serve
		}
		rules[pattern] = rule
	}
	for pattern, next := range peerSpawnPickerRoutes {
		rule := peerViewRule{feature: "spawn.full", requires: PermGroupsMembersSpawnFull, group: true}
		serve := servePeerSpawnDialog(next, func(v *peerView, _ peerViewRule) bool { return peerSpawnFullAnywhere(v.peer.InstanceID) })
		if pattern[:4] == "GET " {
			rule.serve = serve
		} else {
			rule.write = serve
		}
		rules[pattern] = rule
	}
}

// servePeerSpawnDialog runs this node's own dashboard handler for a peer the
// grant check admits, as the dashboard operator.
func servePeerSpawnDialog(next http.HandlerFunc, allowed func(*peerView, peerViewRule) bool) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !allowed(v, rule) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
			return
		}
		next(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
	}
}

// peerSpawnFullAnywhere reports whether the peer may run a full spawn into at
// least one active group.
func peerSpawnFullAnywhere(peer string) bool {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() && fedPeerAllows(peer, g.ID, PermGroupsMembersSpawnFull) {
			return true
		}
	}
	return false
}

// peerBriefSpawnBody is the brief-only spawn request (groups.members.spawn).
type peerBriefSpawnBody struct {
	Name    string `json:"name,omitempty"`
	Role    string `json:"role,omitempty"`
	Brief   string `json:"brief"`
	Profile string `json:"profile,omitempty"`
}

// readPeerSpawnBody reads a spawn body and reports whether it is the
// brief-only request: exactly its fields, nothing else. Any other body is the
// spawn dialog's own, which only groups.members.spawn.full may send.
func readPeerSpawnBody(w http.ResponseWriter, r *http.Request) (raw []byte, brief *peerBriefSpawnBody, ok bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, fedPeerViewBodyLimit))
	if err != nil {
		writeError(w, 400, "invalid_arg", "invalid remote action body")
		return nil, nil, false
	}
	var body peerBriefSpawnBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) == nil && dec.Decode(new(any)) == io.EOF {
		return raw, &body, true
	}
	return raw, nil, true
}

// servePeerFullSpawn runs the spawn dialog's request through this node's
// dashboard spawn handler, as this node's operator would.
func servePeerFullSpawn(w http.ResponseWriter, r *http.Request, v *peerView, g *db.AgentGroup, raw []byte) {
	if !fedPeerAllows(v.peer.InstanceID, g.ID, PermGroupsMembersSpawnFull) {
		writeError(w, 403, "permission", "requires "+PermGroupsMembersSpawnFull)
		return
	}
	v.groupName = g.Name
	v.auditDetail = "full"
	r = r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	groupRoute(dashboardSpawnInGroup)(w, r)
}
