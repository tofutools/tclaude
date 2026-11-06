package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

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

	"GET /v1/process/templates":      PermProcessTemplatesRead,
	"GET /v1/process/templates/{id}": PermProcessTemplatesRead,
	"GET /v1/process/template-heads": PermProcessTemplatesRead,
	"POST /v1/process/validate":      PermProcessTemplatesRead,

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

// peerLibraryAlsoHeld are the gates a library route answers when the peer
// also holds them: a template import's embedded saved prompts take
// prompts.manage.
var peerLibraryAlsoHeld = map[string][]string{
	PermTemplatesManage: {PermPromptsManage},
}

// peerLibraryActor is who a peer's process-template versions are recorded
// as: the operator of the linked node (agent principals reach no library
// route), in the human: form the template store accepts.
func peerLibraryActor(instance string) string {
	return "human:operator@" + strings.ReplaceAll(instance, "_", "-")
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
		for _, slug := range peerLibraryAlsoHeld[rule.requires] {
			if fedPeerHoldsUnscoped(v.peer.InstanceID, slug) {
				gates[slug] = true
			}
		}
		a := &peerGrantAuthority{peer: v.peer.InstanceID, gates: gates, author: peerFollowUpAuthor(v, v.peer.InstanceID)}
		ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), peerGrantKey{}, a)
		ctx = context.WithValue(ctx, processActorKey{}, peerLibraryActor(v.peer.InstanceID))
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
	"model":   fedOverrideModel,
	"effort":  fedOverrideEffort,
	"role":    fedOverrideRoles, "role_ref": fedOverrideRoles, "role_refs": fedOverrideRoles,
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
	"prompt_refs": fedOverridePrompts, "initial_message": fedOverridePrompts, "startup_context": fedOverridePrompts,
	"sync_worktree": fedOverrideWorktree, "fetch_latest_worktree": fedOverrideWorktree,
}

var peerProfileFreeFields = map[string]bool{
	"name": true, "aliases": true, "agent_name": true,
	"auto_focus": true, "disabled": true, "disabled_reason": true, "operator_only": true,
	"created_at": true, "updated_at": true,
}

// peerProfileEditFailure holds a linked node's edit of a spawn profile
// (after nil: its delete) to what that node could launch itself: every
// launch value the edit changes, the operator's default profile's included,
// takes the matching remote spawn grant for every group of this node. A
// profile that starts or stops backing spawns (created, deleted, disabled,
// enabled) or that spawns now reach under another name or alias (by-name
// references in cron jobs, triggers and fallbacks) changes every value it
// sets. Making a profile selectable by agents (clearing operator_only) takes
// unrestricted trust. The local operator and unrestricted trust pass.
func peerProfileEditFailure(r *http.Request, before, after *db.SpawnProfile) *spawnFailure {
	a := peerGrantFromRequest(r)
	if a == nil {
		return nil
	}
	if before != nil && after != nil && before.OperatorOnly && !after.OperatorOnly {
		return &spawnFailure{http.StatusForbidden, "permission", "making a profile selectable by agents takes unrestricted trust"}
	}
	whole := before == nil || after == nil || before.Disabled != after.Disabled || peerProfileRenamed(before, after)
	return peerLaunchChangeFailure(a.peer, wireKeys(profileJSONOf(before)), wireKeys(profileJSONOf(after)), whole,
		peerProfileFields, peerProfileFreeFields, "profile field")
}

func profileJSONOf(p *db.SpawnProfile) any {
	if p == nil {
		return nil
	}
	return profileToJSON(p)
}

func peerProfileRenamed(before, after *db.SpawnProfile) bool {
	if before.Name != after.Name {
		return true
	}
	x, y := slices.Clone(before.Aliases), slices.Clone(after.Aliases)
	slices.Sort(x)
	slices.Sort(y)
	return !slices.Equal(x, y)
}

// peerLaunchChangeFailure judges a change from was to now, as wire keys:
// each launch key that changes (with whole, each one either sets) takes its
// remote spawn field's grant for every group; free keys pass, and any other
// key is refused.
func peerLaunchChangeFailure(peer string, was, now map[string]json.RawMessage, whole bool,
	fields map[string]string, free map[string]bool, what string) *spawnFailure {
	for key := range unionKeys(was, now) {
		if free[key] {
			continue
		}
		if whole {
			if absentJSONValue(was[key]) && absentJSONValue(now[key]) {
				continue
			}
		} else if sameJSONValue(was[key], now[key]) {
			continue
		}
		field, ok := fields[key]
		if !ok {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf("%s %s takes unrestricted trust", what, key)}
		}
		if !peerFieldGrantedEverywhere(peer, field) {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf(
				"%s %s is a launch setting the spawns it backs use: it takes %s and %s for every group",
				what, key, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field])}
		}
	}
	return nil
}

// wireKeys is v's JSON object, by key; nil for nothing. An unreadable value
// reports a key no table knows, so any change to it is refused.
func wireKeys(v any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	if v == nil {
		return out
	}
	raw, err := json.Marshal(v)
	if err != nil || json.Unmarshal(raw, &out) != nil {
		return map[string]json.RawMessage{"unreadable": json.RawMessage(`true`)}
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

// absentJSONValue reports a value that sets nothing: absent, null, "", []
// or {}. false and 0 are values: a true/false setting that is unset
// inherits from a lower tier, so false is a choice.
func absentJSONValue(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "null", `""`, "[]", "{}":
		return true
	}
	return false
}

// sameJSONValue compares two wire values, an absent one equal only to an
// absent one.
func sameJSONValue(a, b json.RawMessage) bool {
	if absentJSONValue(a) || absentJSONValue(b) {
		return absentJSONValue(a) && absentJSONValue(b)
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

// peerTemplateAgentFields maps each template agent key to the remote spawn
// field whose grant opens it: an instantiation launches each agent with
// these. A role's or agent's permission grants take permissions.grant
// instead, and the inline profile is judged key by key as a profile is.
var peerTemplateAgentFields = map[string]string{
	"harness": fedOverrideHarness, "model": fedOverrideModel, "effort": fedOverrideEffort,
	"role": fedOverrideRoles, "role_ref": fedOverrideRoles, "spawn_profile": fedOverrideProfile, "initial_message": fedOverridePrompts,
	"sandbox": fedOverridePermissions, "approval": fedOverridePermissions, "is_owner": fedOverrideOwner,
}

var peerTemplateAgentFreeFields = map[string]bool{
	"name": true, "descr": true, "wave": true, "effective_is_owner": true,
	// Judged on their own below.
	"permissions": true, "profile_inline": true,
}

var peerTemplateFields = map[string]string{"per_agent_worktrees": fedOverrideWorktree, "default_context": fedOverridePrompts}

var peerTemplateFreeFields = map[string]bool{
	"name": true, "descr": true, "work_pattern": true, "process": true,
	"rhythms": true, "wave_max_wait": true, "created_at": true, "updated_at": true, "id": true,
	// Owner scopes have their own gate; agents are judged one by one.
	"owner_scopes": true, "agents": true,
}

// peerTemplateEditFailure holds a linked node's write of a group template
// (before nil: a new one) to what that node could launch itself, as
// peerProfileEditFailure does a profile: an instantiation here launches the
// team the template describes. An agent the write removes launches nothing.
func peerTemplateEditFailure(r *http.Request, before *templateJSON, after templateJSON) *spawnFailure {
	a := peerGrantFromRequest(r)
	if a == nil {
		return nil
	}
	return peerTemplateLaunchFailure(a.peer, before, after)
}

// peerTemplateLaunchFailure judges a template change from before (nil: none)
// to after against what peer could launch itself.
func peerTemplateLaunchFailure(peer string, before *templateJSON, after templateJSON) *spawnFailure {
	var was map[string]json.RawMessage
	if before != nil {
		was = wireKeys(*before)
	}
	if fail := peerLaunchChangeFailure(peer, was, wireKeys(after), before == nil, peerTemplateFields, peerTemplateFreeFields, "template field"); fail != nil {
		return fail
	}
	had := map[string]templateAgentJSON{}
	if before != nil {
		for _, ag := range before.Agents {
			had[ag.Name] = ag
		}
	}
	for _, ag := range after.Agents {
		prev, existed := had[ag.Name]
		var prevKeys map[string]json.RawMessage
		if existed {
			prevKeys = wireKeys(prev)
		}
		what := fmt.Sprintf("template agent %q field", ag.Name)
		if fail := peerLaunchChangeFailure(peer, prevKeys, wireKeys(ag), !existed, peerTemplateAgentFields, peerTemplateAgentFreeFields, what); fail != nil {
			return fail
		}
		if ag.SpawnProfile != "" && (!existed || prev.SpawnProfile != ag.SpawnProfile) && !peerProfileSelectableEverywhere(peer, ag.SpawnProfile) {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf(
				"template agent %q names a spawn profile it could not pick for a spawn into every group", ag.Name)}
		}
		if !sameJSONValue(prevKeys["permissions"], wireKeys(ag)["permissions"]) && !peerGrantsOnlyDropped(prev.Permissions, ag.Permissions) {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf(
				"template agent %q would be spawned with permission grants, which takes %s", ag.Name, PermPermissionsGrant)}
		}
		var prevInline any
		if existed && prev.ProfileInline != nil {
			prevInline = *prev.ProfileInline
		}
		var inline any
		if ag.ProfileInline != nil {
			inline = *ag.ProfileInline
		}
		if fail := peerLaunchChangeFailure(peer, wireKeys(prevInline), wireKeys(inline), !existed || prevInline == nil,
			peerProfileFields, peerProfileFreeFields, fmt.Sprintf("template agent %q inline profile field", ag.Name)); fail != nil {
			return fail
		}
	}
	return nil
}

// peerGrantsOnlyDropped reports whether after holds no grant before did not.
func peerGrantsOnlyDropped(before, after []db.PermissionGrant) bool {
	for _, g := range after {
		if !slices.ContainsFunc(before, func(b db.PermissionGrant) bool { return b.Slug == g.Slug && b.Scope == g.Scope }) {
			return false
		}
	}
	return true
}

// peerFieldGrantedEverywhere reports whether peer may choose field for a
// spawn into every live group of this node, and any later one: a profile or
// template applies its values wherever it is used, and a group-scoped spawn
// grant can narrow a broader field grant for its own group.
func peerFieldGrantedEverywhere(peer, field string) bool {
	if !fedSpawnFieldGranted(peer, 0, field) {
		return false
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() && !fedSpawnFieldGranted(peer, g.ID, field) {
			return false
		}
	}
	return true
}

// peerProfileSelectableEverywhere reports whether peer may pick the profile
// name for a spawn into every live group (checkSelectableProfile): the
// .profile grant opens only profiles that are not operator-only.
func peerProfileSelectableEverywhere(peer, name string) bool {
	if name == "" {
		return true
	}
	if checkSelectableProfile(peer, 0, name) != nil {
		return false
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() && checkSelectableProfile(peer, g.ID, name) != nil {
			return false
		}
	}
	return true
}
