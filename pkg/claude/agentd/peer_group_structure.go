package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A group's structure under peer grants: its members joining, leaving and
// moving, its links to other groups, and its archive and delete. Each runs
// this node's own dashboard handler once the peer layer has checked the
// request, since those handlers trust their caller as this node's operator.
// Unrestricted trust acts as that operator does.
//
// Joining puts an agent under the group's context, settings and owners, so
// adding (or moving in) an agent takes groups.members.add in every group it
// is already in too. Leaving a group can move where an agent's sandbox
// profile and environment come from (the group it joined most recently), so
// a membership change that could hand it another group's sandbox or
// environment takes that group's spawn sandbox or environment grant, as a
// spawn's choice of them does.

func addPeerGroupStructureRules(rules map[string]peerViewRule) {
	for pattern, rule := range map[string]peerViewRule{
		"POST /api/groups/{name}/members":             {feature: "groups.members.add", requires: PermGroupsMembersAdd, group: true},
		"DELETE /api/groups/{name}/members/{conv}":    {feature: "groups.members.remove", requires: PermGroupsMembersRemove, group: true},
		"POST /api/groups/{name}/members/{conv}/move": {feature: "groups.members.remove", requires: PermGroupsMembersRemove, group: true},
		"POST /api/groups/{name}/links":               {feature: "groups.link.add", requires: PermGroupsLinkAdd, group: true},
		"PATCH /api/groups/{name}/links/{id}":         {feature: "groups.link.add", requires: PermGroupsLinkAdd, group: true},
		"DELETE /api/groups/{name}/links/{id}":        {feature: "groups.link.remove", requires: PermGroupsLinkRemove, group: true},
		"POST /api/groups/{name}/archive":             {feature: "groups.archive", requires: PermGroupsArchive, group: true},
		"POST /api/groups/{name}/unarchive":           {feature: "groups.archive", requires: PermGroupsArchive, group: true},
		"DELETE /api/groups/{name}":                   {feature: "groups.delete", requires: PermGroupsDelete, group: true},
	} {
		rule.write = servePeerGroupStructure(pattern)
		rules[pattern] = rule
	}
}

// peerStructureFailure is a refusal: status, code and message.
type peerStructureFailure struct {
	status    int
	code, msg string
}

func peerStructureRefusal(status int, code, format string, args ...any) *peerStructureFailure {
	return &peerStructureFailure{status, code, fmt.Sprintf(format, args...)}
}

var peerStructureChecks = map[string]func(*peerView, *http.Request, *db.AgentGroup, []byte) *peerStructureFailure{
	"POST /api/groups/{name}/members":             peerMemberAddFailure,
	"DELETE /api/groups/{name}/members/{conv}":    peerMemberRemoveFailure,
	"POST /api/groups/{name}/members/{conv}/move": peerMemberMoveFailure,
	"POST /api/groups/{name}/links":               peerLinkAddFailure,
	"PATCH /api/groups/{name}/links/{id}":         peerLinkUpdateFailure,
	"DELETE /api/groups/{name}/links/{id}":        peerLinkRemoveFailure,
	"POST /api/groups/{name}/archive":             peerGroupGrantFailure(PermGroupsArchive),
	"POST /api/groups/{name}/unarchive":           peerUnarchiveFailure,
	"DELETE /api/groups/{name}":                   peerGroupDeleteFailure,
}

// servePeerGroupStructure checks a restricted peer's structure request, then
// runs this node's handler.
func servePeerGroupStructure(pattern string) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	return func(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
		if v.unrestricted() {
			v.groupName = r.PathValue("name")
			peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
			return
		}
		g, err := db.GetAgentGroupByName(r.PathValue("name"))
		// An archived group is out of the peer's view; one the peer may
		// unarchive is found by its archive grant there.
		visible := func(g *db.AgentGroup) bool {
			if g.IsArchived() {
				return pattern == "POST /api/groups/{name}/unarchive" && peerMayUnarchive(v, g)
			}
			return fedPeerGroupVisible(v.peer.InstanceID, g.ID)
		}
		if err != nil || g == nil || !visible(g) {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		v.groupName = g.Name
		body, err := io.ReadAll(io.LimitReader(r.Body, fedPeerViewBodyLimit))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "read body")
			return
		}
		if fail := peerStructureChecks[pattern](v, r, g, body); fail != nil {
			writeError(w, fail.status, fail.code, fail.msg)
			return
		}
		local := peerAsLocalRequest(r)
		local.Body, local.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		peerDashboardMux().ServeHTTP(w, local)
	}
}

func peerGroupGrantFailure(slug string) func(*peerView, *http.Request, *db.AgentGroup, []byte) *peerStructureFailure {
	return func(v *peerView, _ *http.Request, g *db.AgentGroup, _ []byte) *peerStructureFailure {
		return peerHoldsInGroup(v, g, slug)
	}
}

// peerMayUnarchive: an archived group grants nothing (fedPeerAllows), so
// its unarchive is held by the peer's archive grant on it, scoped to it or
// unscoped, read directly.
func peerMayUnarchive(v *peerView, g *db.AgentGroup) bool {
	return fedPeerExplicitGroupGrant(v.peer.InstanceID, g.ID, PermGroupsArchive) != nil
}

func peerUnarchiveFailure(v *peerView, _ *http.Request, g *db.AgentGroup, _ []byte) *peerStructureFailure {
	if g.IsArchived() {
		if !peerMayUnarchive(v, g) {
			return peerStructureRefusal(http.StatusForbidden, "permission", "requires %s for group:%s", PermGroupsArchive, g.Name)
		}
		return nil
	}
	return peerHoldsInGroup(v, g, PermGroupsArchive)
}

func peerHoldsInGroup(v *peerView, g *db.AgentGroup, slug string) *peerStructureFailure {
	if !fedPeerAllows(v.peer.InstanceID, g.ID, slug) {
		return peerStructureRefusal(http.StatusForbidden, "permission", "requires %s for group:%s", slug, g.Name)
	}
	return nil
}

// peerStructureBody decodes a request body into out, requiring every key to
// be one of keys exactly: the handlers' decode folds key case, so a
// look-alike key would stand in for a known one.
func peerStructureBody(body []byte, out any, keys ...string) *peerStructureFailure {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return peerStructureRefusal(http.StatusBadRequest, "invalid_arg", "the body must be a JSON object")
	}
	for key := range raw {
		if !slices.Contains(keys, key) {
			return peerStructureRefusal(http.StatusForbidden, "permission", "%s requires unrestricted trust", key)
		}
	}
	if json.Unmarshal(body, out) != nil {
		return peerStructureRefusal(http.StatusBadRequest, "invalid_arg", "invalid body")
	}
	return nil
}

// peerAgentConv resolves an agent selector to a conversation the peer sees.
func peerAgentConv(v *peerView, selector string) (string, *peerStructureFailure) {
	res, _, err := agent.ResolveSelector(strings.TrimSpace(selector))
	if err != nil || res == nil || !peerAgentActionAllowed(v.peer.InstanceID, res.ConvID, "") {
		return "", peerStructureRefusal(http.StatusNotFound, "not_found", "object not found")
	}
	return res.ConvID, nil
}

// peerJoinFailure: an agent joining a group stays governed as before, so the
// peer must hold groups.members.add in every group it is already in.
func peerJoinFailure(v *peerView, conv string, except int64) *peerStructureFailure {
	names, err := activeGroupNamesForConvs(conv)
	if err != nil {
		return peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the agent's groups")
	}
	for _, name := range names {
		g, err := db.GetAgentGroupByName(name)
		if err != nil || g == nil {
			return peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the agent's groups")
		}
		if g.ID == except {
			continue
		}
		if fail := peerHoldsInGroup(v, g, PermGroupsMembersAdd); fail != nil {
			fail.msg = fmt.Sprintf("the agent joins with the groups it is in: requires %s for group:%s too", PermGroupsMembersAdd, g.Name)
			return fail
		}
	}
	return nil
}

// peerLeaveLaunchFailure: conv leaving left may take its sandbox profile and
// environment from one of next instead (none when next is empty, every
// group's grant then). One that differs from left's takes its spawn grant.
func peerLeaveLaunchFailure(v *peerView, left *db.AgentGroup, next []*db.AgentGroup, keepSandbox, keepEnvironment bool) *peerStructureFailure {
	peer := v.peer.InstanceID
	check := func(field string, differs func(*db.AgentGroup) bool) *peerStructureFailure {
		if len(next) == 0 {
			if differs(&db.AgentGroup{}) && !peerFieldGrantedEverywhere(peer, field) {
				return peerStructureRefusal(http.StatusForbidden, "permission", "leaving its last group changes the agent's %s: it takes %s and %s for every group", field, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field])
			}
			return nil
		}
		for _, g := range next {
			// An archived group grants nothing, yet stays a membership the
			// agent may take its settings from.
			if differs(g) && (g.IsArchived() || !fedSpawnFieldGranted(peer, g.ID, field)) {
				if g.IsArchived() || !fedPeerGroupVisible(peer, g.ID) {
					// A group the peer does not see stays unnamed.
					return peerStructureRefusal(http.StatusForbidden, "permission", "the agent may take its %s from a group you cannot see instead: it takes %s and %s there", field, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field])
				}
				return peerStructureRefusal(http.StatusForbidden, "permission", "the agent may take its %s from group:%s instead: it takes %s and %s there", field, g.Name, PermGroupsMembersSpawn, fedSpawnFieldSlugs[field])
			}
		}
		return nil
	}
	if !keepSandbox {
		if fail := check(fedOverrideSandbox, func(g *db.AgentGroup) bool { return g.SandboxProfile != left.SandboxProfile }); fail != nil {
			return fail
		}
	}
	if !keepEnvironment {
		return check(fedOverrideEnvironment, func(g *db.AgentGroup) bool { return !slices.Equal(g.Environment, left.Environment) })
	}
	return nil
}

// peerRemainingGroups are the groups other than left the agent is a member
// of, archived ones included: the rebind picks its next settings source from
// every membership (db.rebindSandboxResolutionGroup).
func peerRemainingGroups(agentID string, left int64) ([]*db.AgentGroup, *peerStructureFailure) {
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		return nil, peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the agent's groups")
	}
	var out []*db.AgentGroup
	for _, g := range groups {
		if g.ID != left {
			out = append(out, g)
		}
	}
	return out, nil
}

func peerStructureAgentID(conv string) (string, *peerStructureFailure) {
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		return "", peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the agent")
	}
	return agentID, nil
}

func peerMemberAddFailure(v *peerView, _ *http.Request, g *db.AgentGroup, body []byte) *peerStructureFailure {
	if fail := peerHoldsInGroup(v, g, PermGroupsMembersAdd); fail != nil {
		return fail
	}
	var req struct {
		Conv string `json:"conv"`
	}
	if fail := peerStructureBody(body, &req, "conv", "role", "descr"); fail != nil {
		return fail
	}
	conv, fail := peerAgentConv(v, req.Conv)
	if fail != nil {
		return fail
	}
	return peerJoinFailure(v, conv, g.ID)
}

func peerMemberRemoveFailure(v *peerView, r *http.Request, g *db.AgentGroup, _ []byte) *peerStructureFailure {
	if fail := peerHoldsInGroup(v, g, PermGroupsMembersRemove); fail != nil {
		return fail
	}
	conv, fail := peerAgentConv(v, r.PathValue("conv"))
	if fail != nil {
		return fail
	}
	agentID, fail := peerStructureAgentID(conv)
	if fail != nil {
		return fail
	}
	next, fail := peerRemainingGroups(agentID, g.ID)
	if fail != nil {
		return fail
	}
	return peerLeaveLaunchFailure(v, g, next, false, false)
}

func peerMemberMoveFailure(v *peerView, r *http.Request, g *db.AgentGroup, body []byte) *peerStructureFailure {
	if fail := peerHoldsInGroup(v, g, PermGroupsMembersRemove); fail != nil {
		return fail
	}
	var req struct {
		To              string `json:"to"`
		KeepSandbox     bool   `json:"keep_sandbox"`
		KeepEnvironment bool   `json:"keep_environment"`
		KeepPermissions bool   `json:"keep_permissions"`
	}
	if fail := peerStructureBody(body, &req, "to", "keep_sandbox", "keep_environment", "keep_permissions"); fail != nil {
		return fail
	}
	if req.KeepPermissions {
		return peerStructureRefusal(http.StatusForbidden, "permission", "keep_permissions is this node's operator's")
	}
	target, err := db.GetAgentGroupByName(strings.TrimSpace(req.To))
	if err != nil || target == nil || target.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, target.ID) {
		return peerStructureRefusal(http.StatusNotFound, "not_found", "object not found")
	}
	if fail := peerHoldsInGroup(v, target, PermGroupsMembersAdd); fail != nil {
		return fail
	}
	conv, fail := peerAgentConv(v, r.PathValue("conv"))
	if fail != nil {
		return fail
	}
	if fail := peerJoinFailure(v, conv, g.ID); fail != nil {
		return fail
	}
	// A move hands the agent the target's sandbox and environment, unless
	// it keeps the source's.
	return peerLeaveLaunchFailure(v, g, []*db.AgentGroup{target}, req.KeepSandbox, req.KeepEnvironment)
}

// A link lets the source group's members message the target group: it is
// added, or its mode changed, under groups.link.add in both.
func peerLinkAddFailure(v *peerView, _ *http.Request, g *db.AgentGroup, body []byte) *peerStructureFailure {
	var req struct {
		To string `json:"to"`
	}
	if fail := peerStructureBody(body, &req, "to", "mode", "bidir"); fail != nil {
		return fail
	}
	to, err := db.GetAgentGroupByName(strings.TrimSpace(req.To))
	if err != nil || to == nil || to.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, to.ID) {
		return peerStructureRefusal(http.StatusNotFound, "not_found", "object not found")
	}
	for _, side := range []*db.AgentGroup{g, to} {
		if fail := peerHoldsInGroup(v, side, PermGroupsLinkAdd); fail != nil {
			return fail
		}
	}
	return nil
}

// peerGroupLink loads a link of g's by id: links to other nodes (xlink_)
// take unrestricted trust.
func peerGroupLink(v *peerView, g *db.AgentGroup, idStr string) (from, to *db.AgentGroup, fail *peerStructureFailure) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, nil, peerStructureRefusal(http.StatusForbidden, "permission", "a link to another node requires unrestricted trust")
	}
	link, err := db.GetAgentGroupLinkByID(id)
	if err != nil || link == nil || link.FromGroupID != g.ID && link.ToGroupID != g.ID {
		return nil, nil, peerStructureRefusal(http.StatusNotFound, "not_found", "object not found")
	}
	for _, side := range []struct {
		id  int64
		out **db.AgentGroup
	}{{link.FromGroupID, &from}, {link.ToGroupID, &to}} {
		sg, err := db.GetAgentGroupByID(side.id)
		if err != nil || sg == nil || !fedPeerGroupVisible(v.peer.InstanceID, sg.ID) {
			return nil, nil, peerStructureRefusal(http.StatusNotFound, "not_found", "object not found")
		}
		*side.out = sg
	}
	return from, to, nil
}

func peerLinkUpdateFailure(v *peerView, r *http.Request, g *db.AgentGroup, body []byte) *peerStructureFailure {
	var req struct{}
	if fail := peerStructureBody(body, &req, "mode"); fail != nil {
		return fail
	}
	from, to, fail := peerGroupLink(v, g, r.PathValue("id"))
	if fail != nil {
		return fail
	}
	for _, side := range []*db.AgentGroup{from, to} {
		if fail := peerHoldsInGroup(v, side, PermGroupsLinkAdd); fail != nil {
			return fail
		}
	}
	return nil
}

// Removing a link only narrows reach: groups.link.remove in its source.
func peerLinkRemoveFailure(v *peerView, r *http.Request, g *db.AgentGroup, _ []byte) *peerStructureFailure {
	from, _, fail := peerGroupLink(v, g, r.PathValue("id"))
	if fail != nil {
		return fail
	}
	return peerHoldsInGroup(v, from, PermGroupsLinkRemove)
}

// Deleting a group takes groups.delete there. Each member leaves it, and an
// agent that moved away keeping a setting on it loses that setting, so the
// sandbox and environment each may take from its other groups are checked
// as on a remove.
func peerGroupDeleteFailure(v *peerView, _ *http.Request, g *db.AgentGroup, _ []byte) *peerStructureFailure {
	if fail := peerHoldsInGroup(v, g, PermGroupsDelete); fail != nil {
		return fail
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the group's members")
	}
	agents, err := db.AgentsWithSettingsFromGroup(g.ID)
	if err != nil {
		return peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the group's members")
	}
	for _, m := range members {
		agentID, fail := peerStructureAgentID(m.ConvID)
		if fail != nil {
			return fail
		}
		agents = append(agents, agentID)
	}
	slices.Sort(agents)
	for _, agentID := range slices.Compact(agents) {
		next, fail := peerRemainingGroups(agentID, g.ID)
		if fail != nil {
			return fail
		}
		if fail := peerLeaveLaunchFailure(v, g, next, false, false); fail != nil {
			return fail
		}
	}
	return nil
}
