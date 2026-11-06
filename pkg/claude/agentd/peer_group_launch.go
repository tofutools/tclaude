package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A linked peer's operator creates groups with launch settings, deploys and
// reinforces teams from this node's templates, rebriefs a deployed group and
// clones a group (S8b1). Each runs this node's own handler. A restricted peer
// sets, or makes this node launch with, only what its own remote spawn grants
// would let it launch, in every group (peerFieldGrantedEverywhere): a request
// that needs more is refused whole, naming the first missing grant. A
// template agent is judged as a template write is (peerTemplateLaunchFailure),
// whoever wrote the template: an instantiation launches it. Owner seats and
// owner scopes take groups.owners.manage, which no peer grant hands out yet.
// Text the peer gives a team (its task or mission) carries the remote-request
// banner, as a peer spawn's brief does. Unrestricted trust acts as this
// node's operator does.

// peerLaunchBodyFields maps a request key that decides how this node's
// agents launch to the remote spawn fields whose grants open it.
var peerLaunchBodyFields = map[string][]string{
	"cwd":                 {fedOverrideCwd},
	"default_cwd":         {fedOverrideCwd},
	"worktree_path":       {fedOverrideWorktree},
	"worktree_branch":     {fedOverrideWorktree},
	"per_agent_worktrees": {fedOverrideWorktree},
	"environment":         {fedOverrideEnvironment},
	"repository_clone":    {fedOverrideWorktree, fedOverrideCwd},
	"context_override":    {fedOverridePrompts},
	"default_context":     {fedOverridePrompts},
	// A group's remote-control policy overrides its profiles' default.
	"remote_control_policy": {fedOverrideFeatures},
}

// peerLaunchFreeFields name, describe and place the group, or are handled
// on their own (parent, agent_profiles, default_profile, task, mission,
// copy_owners, the clone switches).
var peerLaunchFreeFields = map[string]bool{
	"name": true, "group_name": true, "new_name": true, "descr": true, "descr_override": true,
	"attachment_url": true, "attachment_label": true, "max_members": true, "write_proof_token": true,
	"parent": true, "agent_profiles": true, "default_profile": true, "task": true, "mission": true,
	"copy_owners": true, "no_copy_conv": true, "no_clone_members": true,
}

func addPeerGroupLaunchRules(rules map[string]peerViewRule) {
	for _, pattern := range []string{"POST /api/templates/{name}/instantiate", "POST /api/templates/{name}/deploy"} {
		rules[pattern] = peerViewRule{feature: PermTemplatesUse, requires: PermTemplatesUse, write: servePeerTemplateLaunch(false)}
	}
	rules["POST /api/templates/{name}/reinforce"] = peerViewRule{feature: PermTemplatesUse + ".group", requires: PermTemplatesUse, group: true, write: servePeerTemplateLaunch(true)}
	rules["POST /api/groups/{name}/rebrief"] = withPeerInvoke("POST /api/groups/{name}/rebrief",
		peerViewRule{feature: PermTemplatesUse + ".group", requires: PermTemplatesUse, group: true, invoke: &peerInvokeSpec{param: "name"}})
	rules["POST /api/groups/{name}/clone"] = peerViewRule{feature: PermGroupsClone, requires: PermGroupsClone, group: true, write: servePeerGroupClone}
}

// peerLaunchBody reads a launch request's JSON object.
func peerLaunchBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var body map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &body) != nil || body == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "expected a JSON object")
		return nil, false
	}
	return body, true
}

// peerLaunchBodyFailure judges a launch request's own settings: each one it
// sets takes its remote spawn grants for every group, a parent must be a
// group the peer can see, and any key no table knows is refused.
func peerLaunchBodyFailure(v *peerView, body map[string]json.RawMessage) *spawnFailure {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := body[key]
		if emptyJSONValue(value) {
			continue
		}
		if fields, ok := peerLaunchBodyFields[key]; ok {
			for _, field := range fields {
				if !peerFieldGrantedEverywhere(v.peer.InstanceID, field) {
					return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf(
						"%s decides how this node's agents launch: it takes %s and %s for every group", key, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field])}
				}
			}
			continue
		}
		if !peerLaunchFreeFields[key] {
			return &spawnFailure{http.StatusForbidden, "permission", key + " requires unrestricted trust"}
		}
	}
	var parent string
	if body["parent"] != nil && json.Unmarshal(body["parent"], &parent) != nil || parent != "" && !peerCatalogGroupVisible(parent, v) {
		return &spawnFailure{http.StatusNotFound, "not_found", "object not found"}
	}
	var profile string
	if body["default_profile"] != nil && json.Unmarshal(body["default_profile"], &profile) != nil || !peerProfileSelectableEverywhere(v.peer.InstanceID, profile) {
		return &spawnFailure{http.StatusForbidden, "permission", "default_profile must be a profile it could pick for a spawn into every group"}
	}
	return nil
}

// peerBannerText puts the remote-request banner in front of the request's
// free text for the team (key), as a peer spawn's brief carries it.
func peerBannerText(v *peerView, body map[string]json.RawMessage, key string) {
	var text string
	if body[key] == nil || json.Unmarshal(body[key], &text) != nil || strings.TrimSpace(text) == "" {
		return
	}
	body[key], _ = json.Marshal(peerFollowUpBanner(peerFollowUpAuthor(v, v.peer.InstanceID)) + text)
}

// peerServeAsLocal runs the route's own dashboard handler for the peer
// with the rewritten body, as this node's operator.
func peerServeAsLocal(w http.ResponseWriter, r *http.Request, body map[string]json.RawMessage) {
	raw, _ := json.Marshal(body)
	req := r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
	req.Body = io.NopCloser(strings.NewReader(string(raw)))
	req.ContentLength = int64(len(raw))
	peerDashboardMux().ServeHTTP(w, req)
}

// servePeerTemplateLaunch deploys a team from a template: into a new group
// (instantiate, deploy), or into an existing one (reinforce).
func servePeerTemplateLaunch(into bool) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		if v.unrestricted() {
			peerDashboardMux().ServeHTTP(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
			return
		}
		body, ok := peerLaunchBody(w, r)
		if !ok {
			return
		}
		peer := v.peer.InstanceID
		var group string
		_ = json.Unmarshal(body["group_name"], &group)
		if into {
			g, err := db.GetAgentGroupByName(strings.TrimSpace(group))
			if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(peer, g.ID) {
				writeError(w, http.StatusNotFound, "not_found", "object not found")
				return
			}
			v.groupName = g.Name
			for _, slug := range []string{PermTemplatesUse, PermGroupsMembersSpawn} {
				if !fedPeerAllows(peer, g.ID, slug) {
					writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", slug, g.Name))
					return
				}
			}
		} else {
			// A new group: only grants that hold for every group reach it.
			for _, slug := range []string{PermTemplatesUse, PermNodeGroupsCreate} {
				if !fedPeerHoldsUnscoped(peer, slug) {
					writeError(w, http.StatusForbidden, "permission", "requires "+slug)
					return
				}
			}
			if fedPeerExplicitGroupGrant(peer, 0, PermGroupsMembersSpawn) == nil {
				writeError(w, http.StatusForbidden, "permission", "requires "+PermGroupsMembersSpawn+" for every group")
				return
			}
		}
		if fail := peerLaunchBodyFailure(v, body); fail != nil {
			writeSpawnFailure(w, fail)
			return
		}
		tmpl, err := db.GetGroupTemplate(r.PathValue("name"))
		if err != nil || tmpl == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		if fail := peerTemplateTeamFailure(peer, templateToJSON(tmpl), body["agent_profiles"]); fail != nil {
			writeSpawnFailure(w, fail)
			return
		}
		peerBannerText(v, body, "task")
		peerBannerText(v, body, "mission")
		peerServeAsLocal(w, r, body)
	}
}

// peerTemplateTeamFailure judges the team a template launches, with the
// request's per-agent profile choices laid over it, against what peer could
// launch itself.
func peerTemplateTeamFailure(peer string, t templateJSON, agentProfiles json.RawMessage) *spawnFailure {
	var overrides map[string]string
	if len(agentProfiles) > 0 && !emptyJSONValue(agentProfiles) && json.Unmarshal(agentProfiles, &overrides) != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_arg", "agent_profiles must map agent names to profiles"}
	}
	if len(t.OwnerScopes) > 0 {
		return &spawnFailure{http.StatusForbidden, "permission", "the template sets owner scopes, which take " + PermGroupsOwnersManage}
	}
	for i, ag := range t.Agents {
		if ag.IsOwner || ag.EffectiveIsOwner {
			return &spawnFailure{http.StatusForbidden, "permission", fmt.Sprintf("template agent %q is a group owner, which takes %s", ag.Name, PermGroupsOwnersManage)}
		}
		if name, ok := overrides[ag.Name]; ok {
			t.Agents[i].SpawnProfile = name
		}
	}
	return peerTemplateLaunchFailure(peer, nil, t)
}

// servePeerGroupClone clones a group the peer can see, under groups.clone
// there. Settings copied from the source are this node's operator's own;
// those the request overrides are judged as a new group's are. Cloning its
// members takes groups.members.clone in every group of each, as a member's
// own clone does, and copying its owners takes groups.owners.manage.
func servePeerGroupClone(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	peer := v.peer.InstanceID
	src, err := db.GetAgentGroupByName(r.PathValue("name"))
	if err != nil || src == nil || src.IsArchived() || !fedPeerGroupVisible(peer, src.ID) {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	v.groupName = src.Name
	if !v.allows(rule, src.ID) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", rule.requires, src.Name))
		return
	}
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
		return
	}
	if !fedPeerHoldsUnscoped(peer, PermNodeGroupsCreate) {
		writeError(w, http.StatusForbidden, "permission", "requires "+PermNodeGroupsCreate)
		return
	}
	body, ok := peerLaunchBody(w, r)
	if !ok {
		return
	}
	if fail := peerLaunchBodyFailure(v, body); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	members, err := db.ListAgentGroupMembers(src.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not read the group")
		return
	}
	var noMembers, copyOwners bool
	copyOwners = true
	_ = json.Unmarshal(body["no_clone_members"], &noMembers)
	if body["copy_owners"] != nil {
		_ = json.Unmarshal(body["copy_owners"], &copyOwners)
	}
	if !noMembers {
		for _, m := range members {
			if !peerAgentActionAllowed(peer, m.ConvID, PermGroupsMembersClone) {
				writeError(w, http.StatusForbidden, "permission", "cloning the members takes "+PermGroupsMembersClone+" in every group of each; set no_clone_members to clone the group alone")
				return
			}
		}
	}
	if copyOwners {
		owners, err := db.ListAgentGroupOwners(src.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "could not read the group")
			return
		}
		if len(owners) > 0 {
			writeError(w, http.StatusForbidden, "permission", "copying the group's owners takes "+PermGroupsOwnersManage+"; set copy_owners to false")
			return
		}
	}
	peerServeAsLocal(w, r, body)
}
