package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type peerActionKey struct{}

// This capability is constructed only after dispatcher admission. It never
// grants local-human identity or reads the source agent's own permissions.
type peerActionAuthority struct {
	peer, sourceConv, permission string
	groupID                      int64
}

func peerActionFromRequest(r *http.Request) *peerActionAuthority {
	a, _ := r.Context().Value(peerActionKey{}).(*peerActionAuthority)
	return a
}
func peerAgentActionAllowed(peer, conv, slug string) bool {
	p, err := db.GetFederationPeer(peer)
	if err != nil || p == nil {
		return false
	}
	names, err := activeGroupNamesForConvs(conv)
	if slug == PermGroupsMembersClone || slug == PermGroupsMembersRetire || slug == PermAgentMove {
		names, err = cloneAuthorizationGroups(conv)
	}
	if err != nil || len(names) == 0 {
		return false
	}
	for _, name := range names {
		g, err := db.GetAgentGroupByName(name)
		if err != nil || g == nil || !fedPeerAllows(peer, g.ID, slug) {
			return false
		}
	}
	return true
}
func (a *peerActionAuthority) allows(perm, target string, actx ActionContext) bool {
	if target != "" && target != a.sourceConv {
		return false
	}
	if actx.RemotePeer != "" && actx.RemotePeer != a.peer {
		return false
	}
	if a.groupID != 0 {
		g, err := db.GetAgentGroupByID(a.groupID)
		return err == nil && g != nil && actx.Group == g.Name && perm == PermGroupsMembersSpawn && fedPeerAllows(a.peer, g.ID, PermGroupsMembersSpawn)
	}
	slug := a.permission
	switch perm {
	case PermAgentStop:
		if slug != PermGroupsMembersStop {
			return false
		}
	case PermAgentResume:
		if slug != PermGroupsMembersResume {
			return false
		}
	case PermAgentRetire:
		if slug != PermGroupsMembersRetire && slug != PermAgentMove {
			return false
		}
		slug = PermGroupsMembersRetire
	case PermAgentClone:
		if slug != PermGroupsMembersClone {
			return false
		}
	case PermSelfTeleport, PermAgentMove, PermAgentShare, PermAgentBundleExport:
		if slug != PermAgentMove {
			return false
		}
	default:
		return false
	}
	return peerAgentActionAllowed(a.peer, a.sourceConv, slug)
}
func peerActionGate(w http.ResponseWriter, r *http.Request, a *peerActionAuthority, perm, target string, actx ActionContext) (string, bool) {
	if !a.allows(perm, target, actx) {
		writeError(w, 403, "permission", fmt.Sprintf("requires %s for the complete target footprint", perm))
		return "", false
	}
	recordAuthorizedPermission(r, perm, 0)
	return a.sourceConv, true
}

func servePeerAgentAction(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	target, err := db.GetAgent(r.PathValue("id"))
	if err != nil || target == nil || target.CurrentConvID == "" {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	groups, err := db.ListGroupsForConv(target.CurrentConvID)
	visible := false
	for _, g := range groups {
		if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			visible = true
			v.groupName = g.Name
			break
		}
	}
	if err != nil || !visible {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	if !peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, rule.requires) {
		writeError(w, 403, "permission", fmt.Sprintf("requires %s for every affected group", rule.requires))
		return
	}
	v.targetConv = target.CurrentConvID
	a := &peerActionAuthority{peer: v.peer.InstanceID, sourceConv: target.CurrentConvID, permission: rule.requires}
	r = r.WithContext(context.WithValue(r.Context(), peerActionKey{}, a))
	switch {
	case strings.HasSuffix(r.URL.Path, "/stop"):
		if len(r.URL.Query()) > 0 && (len(r.URL.Query()) != 1 || r.URL.Query().Get("force") != "1") {
			writeError(w, 400, "invalid_arg", "only force=1 is accepted")
			return
		}
		handleAgentStop(w, r, target.CurrentConvID)
	case strings.HasSuffix(r.URL.Path, "/resume"):
		// A peer's resume is the plain wake: recreating a missing launch
		// directory and switching a Codex drive stay local-human decisions.
		if len(r.URL.Query()) > 0 {
			writeError(w, 400, "invalid_arg", "remote resume accepts no options")
			return
		}
		handleAgentResume(w, r, target.CurrentConvID)
	case strings.HasSuffix(r.URL.Path, "/sandbox-restart"), strings.HasSuffix(r.URL.Path, "/restart"):
		// A restart stops the agent first, so it also needs the stop grant.
		if len(r.URL.Query()) > 0 {
			writeError(w, 400, "invalid_arg", "remote restart accepts no options")
			return
		}
		if !peerAgentActionAllowed(a.peer, a.sourceConv, PermGroupsMembersStop) {
			writeError(w, 403, "permission", "requires groups.members.stop and groups.members.resume for every affected group")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/restart") && !strings.HasSuffix(r.URL.Path, "/sandbox-restart") {
			dashboardRestartAgent(w, r, target.CurrentConvID)
			return
		}
		var body struct {
			Action string `json:"action"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		// Only the exact closed values pass: the handler trims, so a padded
		// " unlock" must not slip past the unrestricted check below.
		if body.Action != sandboxRestartUnlock && body.Action != sandboxRestartRestore {
			writeError(w, 400, "invalid_arg", "action must be unlock or restore")
			return
		}
		v.auditDetail = "action=" + body.Action
		// Unlocking runs the agent with its harness sandbox off: only a peer
		// this node trusts unrestricted may ask for that. Restoring is the
		// safe direction and needs only the restart grants.
		if body.Action == sandboxRestartUnlock && !db.FederationPeerUnrestricted(a.peer) {
			writeError(w, 403, "permission", "turning an agent's sandbox off remotely requires unrestricted trust")
			return
		}
		raw, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		dashboardSandboxRestartAgent(w, r, target.CurrentConvID)
	case strings.HasSuffix(r.URL.Path, "/retire"):
		if len(r.URL.Query()) > 0 {
			writeError(w, 400, "invalid_arg", "remote retire does not accept destructive worktree options")
			return
		}
		handleAgentRetire(w, r, target.CurrentConvID)
	case strings.HasSuffix(r.URL.Path, "/clone"):
		var body struct {
			FollowUp   string `json:"follow_up"`
			NoCopyConv bool   `json:"no_copy_conv"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		raw, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		handleAgentClone(w, r, target.CurrentConvID)
	case strings.HasSuffix(r.URL.Path, "/move"), strings.HasSuffix(r.URL.Path, "/teleport"):
		if !peerAgentActionAllowed(a.peer, a.sourceConv, PermGroupsMembersRetire) {
			writeError(w, 403, "permission", "requires groups.members.retire for every affected group")
			return
		}
		var body struct {
			Group            string `json:"group"`
			DirectIfAllowed  bool   `json:"direct_if_allowed"`
			Cwd              string `json:"cwd"`
			Landing          string `json:"landing"`
			Note             string `json:"note,omitempty"`
			Clone            bool   `json:"clone,omitempty"`
			Home             bool   `json:"home,omitempty"`
			CarryPermissions bool   `json:"carry_permissions,omitempty"`
		}
		if !decodePeerAction(w, r, &body) {
			return
		}
		if body.Group == "" {
			writeError(w, 400, "invalid_arg", "receiving group is required")
			return
		}
		if body.Home && !strings.HasSuffix(r.URL.Path, "/teleport") {
			writeError(w, 400, "invalid_arg", "home is only valid for teleport")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/teleport") {
			destination := a.peer
			if body.Home {
				presence, err := db.GetAgentFederationPresence(target.AgentID)
				if err != nil || presence == nil {
					writeError(w, 409, "no_home", "this agent has no recorded home identity")
					return
				}
				home, err := resolveIdentityContinuation(presence.HomeInstance)
				if err != nil || home != a.peer || presence.State != "here" {
					writeError(w, 403, "not_home", "only the recorded home node may recall this agent")
					return
				}
				destination = "" // Home is resolved by the shared teleport path.
			}
			raw, _ := json.Marshal(teleportRequest{Peer: destination, Home: body.Home, Group: body.Group, Note: body.Note, Clone: body.Clone, CarryPermissions: body.CarryPermissions})
			r.Body = io.NopCloser(bytes.NewReader(raw))
			handleFederationTeleport(w, r)
		} else {
			if body.Clone || body.Note != "" {
				writeError(w, 400, "invalid_arg", "move accepts only receiving group")
				return
			}
			raw, _ := json.Marshal(fedShareAgentRequest{Agent: target.AgentID, Peer: a.peer, Group: body.Group, History: true, CarryPermissions: body.CarryPermissions, DirectIfAllowed: body.DirectIfAllowed, Cwd: body.Cwd, Landing: body.Landing})
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.URL.Path = "/v1/federation/move-agent"
			handleFederationShareAgent(w, r)
		}
	}
}
func decodePeerAction(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid_arg", "invalid remote action body")
		return false
	}
	return true
}

// servePeerSpawn takes a peer operator's spawn request into a visible group.
// With a groups.members.spawn grant (or unrestricted trust) it launches
// automatically under the grant's policy; without one it waits for this
// node's operator, exactly like an agent's federation spawn-request.
func servePeerSpawn(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	g, err := db.GetAgentGroupByName(r.PathValue("name"))
	if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) || !v.allows(rule, g.ID) {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	var body struct {
		Name    string `json:"name,omitempty"`
		Role    string `json:"role,omitempty"`
		Brief   string `json:"brief"`
		Profile string `json:"profile,omitempty"`
		Cwd     string `json:"cwd,omitempty"`
		Harness string `json:"harness,omitempty"`
		Model   string `json:"model,omitempty"`
		Effort  string `json:"effort,omitempty"`
		// Roles and SandboxProfile need their groups.members.spawn.* grants.
		Roles          []string `json:"roles,omitempty"`
		SandboxProfile string   `json:"sandbox_profile,omitempty"`
	}
	if !decodePeerAction(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Brief) == "" || len(body.Brief) > proto.MaxSpawnBrief || len(body.Name) > 128 || len(body.Role) > 128 || len(body.Profile) > 128 {
		writeError(w, 400, "invalid_arg", "invalid remote spawn fields")
		return
	}
	overrides := db.FederationSpawnOverrides{Cwd: body.Cwd, Harness: body.Harness, Model: body.Model, Effort: body.Effort, Roles: body.Roles, SandboxProfile: body.SandboxProfile}
	if err := validateFedSpawnOverrides(overrides); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	if err := checkFedSpawnOverrides(v.peer.InstanceID, g.ID, overrides); err != nil {
		writeError(w, 403, "override_not_allowed", err.Error())
		return
	}
	if err := checkSelectableProfile(v.peer.InstanceID, g.ID, body.Profile); err != nil {
		writeError(w, 403, "profile_not_allowed", err.Error())
		return
	}
	pinnedProfile := ""
	if grant := fedPeerGroupGrant(v.peer.InstanceID, g.ID, PermGroupsMembersSpawn); grant != nil {
		pinnedProfile = grant.SpawnPolicy.Profile
	}
	if err := checkFedHarnessProfile(fedFirst(body.Profile, pinnedProfile), overrides.Harness); err != nil {
		writeError(w, 409, "harness_profile_conflict", err.Error()+"; choose that harness or leave the harness blank")
		return
	}
	req := &db.FederationSpawnRequest{FromInstance: v.peer.InstanceID, FromName: "operator", EnvelopeID: proto.NewEnvelopeID(), GroupID: g.ID, GroupName: g.Name, Name: proto.SafeName(body.Name, false), Role: proto.SafeName(body.Role, false), Brief: body.Brief, Profile: body.Profile, Overrides: overrides, ExpiresAt: time.Now().Add(fedSpawnTTL)}
	id, err := insertFederationSpawnWithCapacity(req)
	if err != nil {
		writeError(w, 409, "spawn_capacity", "remote spawn request capacity unavailable")
		return
	}
	req.ID = id
	req.Status = db.FedSpawnPending
	req.CreatedAt = time.Now().UTC()
	v.groupName = g.Name
	if fedPeerAllows(v.peer.InstanceID, g.ID, PermGroupsMembersSpawn) {
		goBackground(func() { autoApproveFederationSpawnWithPeerAction(req, v.peer, true) })
	} else {
		notifyFederationSpawnRequest(v.peer, req, "operator")
	}
	writeJSON(w, 202, fedSpawnRequestView(req, time.Now()))
}

// peerSpawnOptions tells a peer's spawn dialog what it may choose for one
// group. Receiver-owned values (the grant's pinned settings, group defaults)
// are named only to a peer holding node.spawn.read (unrestricted trust holds
// it), which can read them from the catalogs anyway.
type peerSpawnOptions struct {
	Group string `json:"group"`
	// Approval is "automatic" (a spawn grant or unrestricted trust launches
	// it) or "operator" (it waits for this node's operator).
	Approval  string                      `json:"approval"`
	Overrides map[string]bool             `json:"overrides"`
	Profiles  []proto.CatalogSpawnProfile `json:"profiles"`
	Harnesses []string                    `json:"harnesses,omitempty"`
	Defaults  map[string]string           `json:"defaults,omitempty"`
	// Catalog reports node.spawn.read: the requester may read this node's
	// spawn dialog catalogs and pre-fill the dialog with what it would apply.
	Catalog bool `json:"catalog"`
	// DefaultPrompts are the group's own saved prompts (with the group's
	// default profile in Defaults["group_profile"]): named with Catalog, so
	// the dialog pre-fills them whatever the peer-view snapshot shares.
	DefaultPrompts []string `json:"default_prompts,omitempty"`
}

func servePeerSpawnOptions(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	g, err := db.GetAgentGroupByName(r.PathValue("name"))
	if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) || !v.allows(rule, g.ID) {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	out := peerSpawnOptions{Group: g.Name, Approval: "operator", Overrides: fedSpawnOverridesAllowed(v.peer.InstanceID, g.ID), Profiles: selectableSpawnProfiles(v.peer.InstanceID, g.ID)}
	for _, field := range fedSpawnOverrideFields {
		if _, ok := out.Overrides[field]; !ok {
			out.Overrides[field] = false // every field is named, open or not
		}
	}
	if out.Profiles == nil {
		out.Profiles = []proto.CatalogSpawnProfile{}
	}
	grant := fedPeerGroupGrant(v.peer.InstanceID, g.ID, PermGroupsMembersSpawn)
	if grant != nil {
		out.Approval = "automatic"
	}
	if out.Overrides[fedOverrideHarness] {
		out.Harnesses = fedSpawnHarnesses()
	}
	out.Catalog = peerSpawnCatalogReadable(v)
	if out.Catalog {
		out.Defaults = map[string]string{"cwd": g.DefaultCwd, "group_profile": g.DefaultProfile}
		out.DefaultPrompts = g.DefaultPrompts
		if grant != nil {
			for k, val := range map[string]string{"cwd": grant.SpawnPolicy.Cwd, "harness": grant.SpawnPolicy.Harness, "model": grant.SpawnPolicy.Model, "profile": grant.SpawnPolicy.Profile} {
				if val != "" {
					out.Defaults[k] = val
				}
			}
		}
	}
	v.groupName = g.Name
	writeJSON(w, 200, out)
}

func servePeerSpawnStatus(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 404, "not_found", "spawn request not found")
		return
	}
	req, err := db.GetFederationSpawnRequest(id)
	if err != nil || req == nil || req.FromInstance != v.peer.InstanceID {
		writeError(w, 404, "not_found", "spawn request not found")
		return
	}
	if !v.allows(rule, req.GroupID) {
		writeError(w, 404, "not_found", "spawn request not found")
		return
	}
	writeJSON(w, 200, fedSpawnRequestView(req, time.Now()))
}

// Sender audits use the same mapping as receiving authorization. Only known
// route patterns enter the log; peer-controlled bodies/selectors never do.
func auditPeerActionProxy(r *http.Request, peer, phase string, status int) {
	mux := http.NewServeMux()
	for pattern, rule := range peerViewRules() {
		if rule.write == nil || rule.feature != "messaging" && rule.feature != "spawn.request" && rule.feature != "human.inbox.answer" && !strings.HasPrefix(rule.feature, "lifecycle.") {
			continue
		}
		mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}
	probe := &http.Request{Method: r.Method, URL: &url.URL{Path: "/api/" + r.PathValue("tail")}, Header: make(http.Header)}
	_, pattern := mux.Handler(probe)
	if pattern == "" {
		return
	}
	recordFederationAudit("federation.peer_view.out", "operator", "", "", fmt.Sprintf("phase=%s peer=%s route=%s", phase, peer, pattern), status)
}
