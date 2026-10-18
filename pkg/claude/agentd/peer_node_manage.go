package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The Groups toolbar over the peer view (docs/federation.md, "Managing a
// peer's node"): a peer page sets this node's global spawn and sandbox
// defaults and creates groups through this node's own dashboard handlers, so
// a remote node with the right grants feels like the local one.
const (
	// PermNodeSpawnDefaultsManage lets a peer set or clear this node's global
	// default spawn profile and global sandbox profile, choosing among the
	// profiles this node already has. Instance-wide.
	PermNodeSpawnDefaultsManage = "node.spawn.defaults.manage"
	// PermNodeSpawnProfilesCreate lets a peer save a new spawn profile on
	// this node (the spawn dialog's "Save as profile…"). Instance-wide. A
	// restricted peer's profile is saved operator-only, so it never opens
	// a launch setting the peer's spawn grants keep closed.
	PermNodeSpawnProfilesCreate = "node.spawn.profiles.create"
	// PermNodeGroupsCreate lets a peer create plain groups on this node.
	// Instance-wide. A restricted peer's group carries no launch settings
	// (directory, environment, profile, repository clone); unrestricted trust
	// creates any group, from a template or as a clone too.
	PermNodeGroupsCreate = "node.groups.create"
)

func addPeerNodeManageRules(rules map[string]peerViewRule) {
	for _, pattern := range []string{"PUT /api/spawn-profile-default", "DELETE /api/spawn-profile-default"} {
		rules[pattern] = peerViewRule{feature: "spawn.defaults", requires: PermNodeSpawnDefaultsManage, write: servePeerGrantedWrite(dashboardSpawnProfilesRoute(handleGlobalDefaultSpawnProfile))}
	}
	for _, pattern := range []string{"PUT /api/sandbox-profile-default", "DELETE /api/sandbox-profile-default"} {
		rules[pattern] = peerViewRule{feature: "spawn.defaults", requires: PermNodeSpawnDefaultsManage, write: servePeerGrantedWrite(dashboardSandboxProfilesRoute(handleGlobalSandboxProfile))}
	}
	rules["POST /api/spawn-profiles"] = peerViewRule{feature: "spawn.profiles.create", requires: PermNodeSpawnProfilesCreate, write: servePeerProfileCreate}
	rules["POST /api/groups"] = peerViewRule{feature: "groups.create", requires: PermNodeGroupsCreate, write: servePeerGroupCreate}
	// A template's team and a clone's members spawn on this node with its
	// launch settings: only unrestricted trust holds these.
	rules["POST /api/templates/{name}/instantiate"] = peerViewRule{feature: "groups.create.full", requires: PermNodeGroupsCreate, unrestrictedOnly: true, write: servePeerGrantedWrite(dashboardTemplatesRoute(handleTemplateInstantiate))}
	rules["POST /api/groups/{name}/clone"] = peerViewRule{feature: "groups.create.full", requires: PermNodeGroupsCreate, unrestrictedOnly: true, write: servePeerGrantedWrite(groupRoute(handleDashboardGroupClone))}
	// A group's ⚙ delete (with retiring its members first) and cleanup of its
	// offline members remove what no peer grant hands out: only unrestricted
	// trust holds these.
	manage := func(next http.HandlerFunc) peerViewRule {
		return peerViewRule{feature: "groups.manage.full", requires: PermGroupsDelete, unrestrictedOnly: true, write: servePeerFullGroup(next)}
	}
	rules["DELETE /api/groups/{name}"] = manage(groupRoute(func(w http.ResponseWriter, _ *http.Request, g *db.AgentGroup) {
		dashboardDeleteGroup(w, g)
	}))
	rules["POST /api/groups/{name}/retire"] = manage(groupRoute(dashboardGroupRetire))
	rules["POST /api/cleanup/group"] = manage(handleDashboardCleanup)
}

// servePeerGrantedWrite runs a local dashboard mutation for a peer holding the
// rule's instance grant; anyone else gets a 403 naming it.
func servePeerGrantedWrite(next http.HandlerFunc) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !v.allows(rule, 0) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
			return
		}
		next(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
	}
}

// servePeerProfileCreate saves a new spawn profile for a peer holding
// node.spawn.profiles.create. A restricted peer's profile is operator-only:
// a profile carries roles, permissions, environment and sandbox modes, and
// a peer allowed to pick any profile (groups.members.spawn.profile) must not
// reach those through one it wrote itself. This node's operator can lift it.
func servePeerProfileCreate(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	if !db.FederationPeerUnrestricted(v.peer.InstanceID) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		var body map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &body) != nil || body == nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "expected a JSON object")
			return
		}
		body["operator_only"] = json.RawMessage("true")
		raw, _ = json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
	}
	dashboardSpawnProfilesRoute(handleSpawnProfiles)(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
}

// peerGroupCreateFields are what a restricted peer may set on a new group:
// its identity and description, never a launch setting a later spawn into it
// would apply.
var peerGroupCreateFields = map[string]bool{
	"name": true, "parent": true, "descr": true, "default_context": true,
	"attachment_url": true, "attachment_label": true, "max_members": true,
}

func servePeerGroupCreate(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	if !db.FederationPeerUnrestricted(v.peer.InstanceID) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		var body map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &body) != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "expected a JSON object")
			return
		}
		for key, value := range body {
			if !peerGroupCreateFields[key] && !emptyJSONValue(value) {
				writeError(w, http.StatusForbidden, "permission", key+" requires unrestricted trust")
				return
			}
		}
		// A subgroup nests under a group the peer can see.
		var parent string
		if body["parent"] != nil && json.Unmarshal(body["parent"], &parent) != nil || parent != "" && !peerCatalogGroupVisible(parent, v) {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
	}
	handleDashboardGroupsCreate(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
}

// emptyJSONValue reports a field the dashboard sends unset: null, "", 0,
// false, [] or {}.
func emptyJSONValue(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "null", `""`, "0", "false", "[]", "{}":
		return true
	}
	return false
}
