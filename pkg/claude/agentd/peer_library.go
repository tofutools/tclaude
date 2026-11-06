package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A linked peer's operator authors this node's libraries (S8a): saved
// prompts, roles, group templates, spawn profiles and process templates, each
// under its own instance-wide peer grant. The route runs this node's own
// handler under a node-wide peerGrantAuthority, so every permission gate
// inside it answers from the peer's grants: the route's own slug passes, and
// any other gate the handler reaches (a template's owner scopes, say) is
// refused, as no peer grant hands it out. What the handlers otherwise judge
// against a local caller only is judged here against the peer:
//
//   - a role's new grants take permissions.grant (roleConferralFailureFor);
//   - a spawn profile changes only to what the peer could launch with
//     itself (peerProfileEditFailure);
//   - a template import that embeds profiles or roles takes unrestricted
//     trust (handleTemplateImport).
//
// Unrestricted trust acts as this node's operator does.

// peerLibraryRoutes maps each library route a peer may call to the peer
// grant it needs. Reads of the prompt, role, template and profile libraries
// are node.spawn.read's (peer_spawn_catalog.go).
var peerLibraryRoutes = map[string]string{
	"POST /api/saved-prompts":          PermPromptsManage,
	"PATCH /api/saved-prompts/{name}":  PermPromptsManage,
	"DELETE /api/saved-prompts/{name}": PermPromptsManage,

	"POST /api/roles":          PermRolesManage,
	"PATCH /api/roles/{name}":  PermRolesManage,
	"DELETE /api/roles/{name}": PermRolesManage,

	"POST /api/templates":            PermTemplatesManage,
	"PATCH /api/templates/{name}":    PermTemplatesManage,
	"DELETE /api/templates/{name}":   PermTemplatesManage,
	"POST /api/templates/from-group": PermTemplatesManage,
	"POST /api/templates/import":     PermTemplatesManage,

	// Creating one is node.spawn.profiles.create's (peer_node_manage.go).
	"PATCH /api/spawn-profiles/{name}":  PermProfilesManage,
	"DELETE /api/spawn-profiles/{name}": PermProfilesManage,

	"GET /v1/process/templates":             PermProcessTemplatesRead,
	"GET /v1/process/templates/{id}":        PermProcessTemplatesRead,
	"GET /v1/process/template-heads":        PermProcessTemplatesRead,
	"GET /v1/process/templates/{id}/export": PermProcessTemplatesRead,
	"POST /v1/process/validate":             PermProcessTemplatesRead,

	"POST /v1/process/templates":              PermProcessTemplatesManage,
	"POST /v1/process/templates/{id}":         PermProcessTemplatesManage,
	"DELETE /v1/process/templates/{id}":       PermProcessTemplatesManage,
	"POST /v1/process/templates/{id}/listing": PermProcessTemplatesManage,
	"POST /v1/process/import":                 PermProcessTemplatesManage,
}

// peerLibraryImplied are the gates a library grant also answers inside its
// routes: managing process templates reads them too, as a save reads the
// head it replaces.
var peerLibraryImplied = map[string][]string{
	PermProcessTemplatesManage: {PermProcessTemplatesRead},
}

func addPeerLibraryRules(rules map[string]peerViewRule) {
	for pattern, slug := range peerLibraryRoutes {
		rule := peerViewRule{feature: slug, requires: slug}
		if pattern == "POST /api/templates/from-group" {
			rule.write = servePeerLibrary(peerTemplateFromGroupVisible)
		} else if len(pattern) > 4 && pattern[:4] == "GET " {
			rule.serve = servePeerLibrary(nil)
		} else {
			rule.write = servePeerLibrary(nil)
		}
		rules[pattern] = rule
	}
	// Pinning a template to a group's process list changes that group: it
	// also takes groups.settings there.
	for _, pattern := range []string{"POST /v1/process/groups/{group}/processes", "DELETE /v1/process/groups/{group}/processes/{templateId}"} {
		rules[pattern] = withPeerInvoke(pattern, peerViewRule{feature: "process.templates.pin", requires: PermGroupsSettings, group: true,
			invoke: &peerInvokeSpec{param: "group", extra: peerGroupPinGates}})
	}
}

// peerGroupPinGates opens the pin's own gate for a peer that also manages
// this node's process templates.
func peerGroupPinGates(peer string, _ *db.AgentGroup) []string {
	if db.FederationPeerUnrestricted(peer) || fedPeerHoldsUnscoped(peer, PermProcessTemplatesManage) {
		return []string{PermProcessTemplatesManage}
	}
	return nil
}

// servePeerLibrary runs a library route for a peer holding its grant.
// guard, when set, refuses a request naming what the peer cannot see.
func servePeerLibrary(guard func(*http.Request, *peerView) bool) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if !v.allows(rule, 0) {
			writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
			return
		}
		if v.unrestricted() {
			peerDashboardMux().ServeHTTP(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
			return
		}
		if guard != nil && !guard(r, v) {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		gates := map[string]bool{rule.requires: true}
		for _, slug := range peerLibraryImplied[rule.requires] {
			gates[slug] = true
		}
		a := &peerGrantAuthority{peer: v.peer.InstanceID, gates: gates, author: peerFollowUpAuthor(v, v.peer.InstanceID)}
		ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), peerGrantKey{}, a)
		peerDashboardMux().ServeHTTP(w, r.Clone(ctx))
	}
}

// peerTemplateFromGroupVisible: a template is snapshotted from a group the
// peer can see.
func peerTemplateFromGroupVisible(r *http.Request, v *peerView) bool {
	var body struct {
		Group string `json:"group"`
	}
	raw, ok := peekJSONBody(r, 1<<20)
	if !ok || json.Unmarshal(raw, &body) != nil {
		return false
	}
	return peerCatalogGroupVisible(body.Group, v)
}

// peerProfileFields maps each spawn profile key to the remote spawn field
// whose grant opens it (federation_spawn_overrides.go): a value a profile
// sets is one every spawn it backs launches with. Keys in
// peerProfileFreeFields change nothing a spawn launches with; any other key
// is refused, so a new profile field stays closed until it is mapped.
var peerProfileFields = map[string]string{
	"harness": fedOverrideHarness,
	"model":   fedOverrideModel, "model_proxy": fedOverrideModel,
	"effort": fedOverrideEffort,
	"role":   fedOverrideRoles, "role_ref": fedOverrideRoles, "role_refs": fedOverrideRoles,
	"sandbox": fedOverridePermissions, "sandbox_implementation": fedOverridePermissions,
	"approval": fedOverridePermissions, "tools": fedOverridePermissions,
	"ask_user_question_timeout": fedOverridePermissions, "auto_review": fedOverridePermissions,
	"trust_dir": fedOverridePermissions, "permission_overrides": fedOverridePermissions,
	"relaunch_policy":     fedOverridePermissions,
	"environment":         fedOverrideEnvironment,
	"is_owner":            fedOverrideOwner,
	"auto_compact_window": fedOverrideFeatures, "context_window_max": fedOverrideFeatures,
	"copilot_api": fedOverrideFeatures, "codex_app_server": fedOverrideFeatures,
	"fast_mode": fedOverrideFeatures, "auto_memory": fedOverrideFeatures,
	"peer_messaging": fedOverrideFeatures, "ssh_workaround": fedOverrideFeatures,
	"remote_control": fedOverrideFeatures, "context_features": fedOverrideFeatures,
	"descr": fedOverrideFeatures, "include_group_default_context": fedOverrideFeatures,
	"prompt_refs":   fedOverridePrompts,
	"sync_worktree": fedOverrideWorktree, "fetch_latest_worktree": fedOverrideWorktree,
}

var peerProfileFreeFields = map[string]bool{
	"name": true, "aliases": true, "agent_name": true, "initial_message": true, "startup_context": true,
	"auto_focus": true, "disabled": true, "disabled_reason": true, "operator_only": true,
	"created_at": true, "updated_at": true,
}

// peerProfileEditFailure holds a linked node's edit of a spawn profile
// (after nil: its delete) to what that node could launch itself: every
// launch value the edit changes, the operator's default profile's included,
// takes the matching remote spawn grant for every group of this node. A
// disabled profile backs no spawn, so disabling or deleting one changes
// every value it set, and enabling one sets them all. Making a profile
// selectable by agents (clearing operator_only) takes unrestricted trust.
// The local operator and unrestricted trust pass.
func peerProfileEditFailure(r *http.Request, before, after *db.SpawnProfile) *spawnFailure {
	a := peerGrantFromRequest(r)
	if a == nil {
		return nil
	}
	if before != nil && after != nil && before.OperatorOnly && !after.OperatorOnly {
		return &spawnFailure{http.StatusForbidden, "permission", "making a profile selectable by agents takes unrestricted trust"}
	}
	was, now := peerProfileLaunch(before), peerProfileLaunch(after)
	for key := range unionKeys(was, now) {
		if peerProfileFreeFields[key] || sameJSONValue(was[key], now[key]) {
			continue
		}
		field, ok := peerProfileFields[key]
		if !ok {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf("profile field %s takes unrestricted trust", key)}
		}
		if !fedSpawnFieldGranted(a.peer, 0, field) {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf(
				"profile field %s is a launch setting every spawn it backs uses: it takes %s and %s for every group",
				key, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field])}
		}
	}
	return nil
}

// peerProfileLaunch is what p sets for the spawns it backs, as wire keys:
// nothing when there is no profile or it is disabled.
func peerProfileLaunch(p *db.SpawnProfile) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if p == nil || p.Disabled {
		return out
	}
	raw, err := json.Marshal(profileToJSON(p))
	if err != nil || json.Unmarshal(raw, &out) != nil {
		// Unreadable: report every launch field as set, so any change is
		// judged.
		for key := range peerProfileFields {
			out[key] = json.RawMessage(`"?"`)
		}
	}
	return out
}

func unionKeys(a, b map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// sameJSONValue compares two wire values, an absent one equal to an empty
// one.
func sameJSONValue(a, b json.RawMessage) bool {
	if a == nil || emptyJSONValue(a) {
		return b == nil || emptyJSONValue(b)
	}
	if b == nil || emptyJSONValue(b) {
		return false
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	xs, _ := json.Marshal(x)
	ys, _ := json.Marshal(y)
	return string(xs) == string(ys)
}

// peekJSONBody reads up to limit bytes of r's body and puts them back for
// the handler.
func peekJSONBody(r *http.Request, limit int64) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	return raw, true
}
