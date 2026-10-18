package agentd

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Peer-granted dashboard routes: a trusted peer's operator changes this
// node's groups through the very dashboard handlers this node's own operator
// uses, so a control that works here works from a peer's dashboard too. The
// route is declared once, in peerViewRules (a peerInvoke rule), with the peer
// grant it needs; everything else is the existing local handler and its own
// permission gates.
//
// Two checks hold for every such request:
//   - before the handler runs, the peer must hold the rule's grant for the
//     target group (so a handler without gates of its own is still covered);
//   - inside it, every requirePermission gate passes only for the local slugs
//     the peer's grants cover for that one group (peerGrantFamilies), so a
//     PATCH that also touches member permissions is refused field by field,
//     exactly like an agent caller's.
//
// The peer is never the local human: asDashboardHumanPeer leaves such a
// request unidentified, so a human-only check refuses it.

const (
	// PermGroupsSettings is the federation peer grant (group-scoped) for a
	// group's ordinary settings: description, startup context, member cap,
	// notifications, remote-control policy, attachment, name and nesting; its
	// default directory, profile, saved prompts and sandbox profile also need
	// the matching spawn grant. Unrestricted trust holds it.
	PermGroupsSettings = "groups.settings"
)

// peerGrantFamilies maps each peer grant to the local gate slugs it
// satisfies within its group. A grant names what an operator decides as one
// unit, so a peer is not handed one slug per control. The environment stays
// its own grant: its values reach every spawned agent's process. Settings
// that every spawn into the group inherits need the peer's say over that
// part of a spawn as well (peerGrantGates, handleGroupUpdate).
var peerGrantFamilies = map[string][]string{
	PermGroupsSettings: {
		PermGroupsSettingsDescription, PermGroupsSettingsDefaultDir, PermGroupsSettingsDefaultContext,
		PermGroupsSettingsDefaultProfile, PermGroupsSettingsMaxMembers, PermGroupsSettingsNotifications,
		PermGroupsSettingsRemoteControlPolicy, PermGroupsAttachment, PermGroupsRename, PermGroupsNest,
	},
	PermGroupsSettingsEnvironment: {PermGroupsSettingsEnvironment},
}

// peerInvokeSpec marks a dashboard route a peer's operator may call.
type peerInvokeSpec struct {
	// param is the path wildcard naming the target group.
	param string
	// extra adds gate slugs this route needs beyond the families, once the
	// peer holds what it takes (nil: none).
	extra func(peer string, g *db.AgentGroup) []string
}

type peerGrantKey struct{}

// peerGrantAuthority is the caller of a peer-granted route: the peer, the
// one group the route targets, and the local gate slugs its grants cover
// there. It is computed once per request.
type peerGrantAuthority struct {
	peer  string
	group *db.AgentGroup
	gates map[string]bool
}

func peerGrantFromRequest(r *http.Request) *peerGrantAuthority {
	a, _ := r.Context().Value(peerGrantKey{}).(*peerGrantAuthority)
	return a
}

// peerGrantGates lists the local gate slugs peer's grants cover in group.
func peerGrantGates(peer string, groupID int64) map[string]bool {
	gates := map[string]bool{}
	for grant, slugs := range peerGrantFamilies {
		if !fedPeerAllows(peer, groupID, grant) {
			continue
		}
		for _, slug := range slugs {
			gates[slug] = true
		}
	}
	// The default directory is every spawn's launch directory: it also needs
	// the grant to choose a spawn's directory. (A default profile is checked
	// per profile: only one the peer could pick for a spawn there.)
	if !peerSpawnFieldHeld(peer, groupID, PermGroupsMembersSpawnCwd) {
		delete(gates, PermGroupsSettingsDefaultDir)
	}
	return gates
}

// peerSpawnFieldHeld reports a groups.members.spawn.* launch-setting grant
// (or the all-fields grant) in group; unrestricted trust holds them all.
func peerSpawnFieldHeld(peer string, groupID int64, slug string) bool {
	return fedPeerAllows(peer, groupID, slug) || fedPeerAllows(peer, groupID, PermGroupsMembersSpawnAll)
}

// allowsDefaultProfile reports whether a peer may make name the group's
// default spawn profile: only one it could select for a spawn into the group
// (checkSelectableProfile), since every spawn there inherits it.
func (a *peerGrantAuthority) allowsDefaultProfile(name string) error {
	if name == "" {
		return nil
	}
	return checkSelectableProfile(a.peer, a.group.ID, name)
}

// allowsDefaultPrompts reports whether a peer may set the group's default
// saved prompts, which open every spawn's brief there: only with the grant
// to choose a spawn's saved prompts.
func (a *peerGrantAuthority) allowsDefaultPrompts() bool {
	return peerSpawnFieldHeld(a.peer, a.group.ID, PermGroupsMembersSpawnPrompts)
}

// allowsParent reports whether a peer may nest the group under parent: one
// of the groups this node shows it (blank clears the parent).
func (a *peerGrantAuthority) allowsParent(parent string) bool {
	if parent == "" {
		return true
	}
	p, err := db.GetAgentGroupByName(parent)
	return err == nil && p != nil && !p.IsArchived() && fedPeerGroupVisible(a.peer, p.ID)
}

// gate is requirePermissionEx for a peer-granted request: the slug must be
// one the peer's grants cover, and a gate naming a group must name the
// route's own.
func (a *peerGrantAuthority) gate(w http.ResponseWriter, r *http.Request, perm string, actx ActionContext) (string, bool) {
	other := func(name string) bool { return name != "" && name != a.group.Name }
	if !a.gates[perm] || other(actx.Group) || other(actx.structuralGroup) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("this node does not grant you %s for group:%s", perm, a.group.Name))
		return "", false
	}
	recordAuthorizedPermission(r, perm, 0)
	return "", true
}

// peerGroupSandboxGates lets a group's sandbox profile be assigned by a peer
// that may also choose a spawn's sandbox profile there: a profile can widen
// what the group's agents reach, as an explicit spawn sandbox can.
func peerGroupSandboxGates(peer string, g *db.AgentGroup) []string {
	if peerSpawnFieldHeld(peer, g.ID, PermGroupsMembersSpawnSandbox) {
		return []string{PermSandboxProfilesManage}
	}
	return nil
}

// peerDashboardMux is this node's dashboard route table, built once, which
// peer-granted requests are dispatched into.
var (
	peerDashboardMuxOnce  sync.Once
	peerDashboardMuxValue *http.ServeMux
)

func peerDashboardMux() *http.ServeMux {
	peerDashboardMuxOnce.Do(func() {
		peerDashboardMuxValue = http.NewServeMux()
		registerDashboardRoutes(peerDashboardMuxValue)
	})
	return peerDashboardMuxValue
}

// servePeerInvoke runs a peer-granted dashboard route (see the file comment).
func servePeerInvoke(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	g, err := db.GetAgentGroupByName(r.PathValue(rule.invoke.param))
	if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	v.groupName = g.Name
	if !v.allows(rule, g.ID) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", rule.requires, g.Name))
		return
	}
	a := &peerGrantAuthority{peer: v.peer.InstanceID, group: g, gates: peerGrantGates(v.peer.InstanceID, g.ID)}
	if rule.invoke.extra != nil {
		for _, slug := range rule.invoke.extra(v.peer.InstanceID, g) {
			a.gates[slug] = true
		}
	}
	ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), peerGrantKey{}, a)
	peerDashboardMux().ServeHTTP(w, r.Clone(ctx))
}

// peerGroupControlGrants are the grants behind a group's peer-invoked
// controls, which peer_view.groups names per group.
var peerGroupControlGrants = []string{PermGroupsSettings, PermGroupsSettingsEnvironment, PermGroupsMembersStop, PermGroupsMembersResume}

// peerGroupGrants names, per group, the peer grants of
// peerGroupControlGrants (and the spawn directory, prompts and sandbox
// grants a group's default directory, saved prompts and sandbox assignment
// also need) that peer holds there, so its dashboard enables exactly the
// controls a click would be allowed to run. It rides on every peer-view read,
// so it lists the peer's grants once. Unrestricted trust holds them all on every visible
// group, which the metadata's level already says: nothing is listed.
func peerGroupGrants(peer string, groups []*db.AgentGroup) map[string][]string {
	if db.FederationPeerUnrestricted(peer) {
		return nil
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return nil
	}
	holds := func(groupID int64, slug string) bool {
		for _, g := range grants {
			if g.Slug == slug && (g.Scope == "" || g.Scope == db.FederationGroupScope(groupID)) {
				return true
			}
		}
		return false
	}
	out := map[string][]string{}
	for _, g := range groups {
		// A settings grant is a known peer slug, so holding one makes the
		// group visible to the peer as well.
		if g.IsArchived() {
			continue
		}
		var held []string
		for _, slug := range peerGroupControlGrants {
			if holds(g.ID, slug) {
				held = append(held, slug)
			}
		}
		if len(held) == 0 {
			continue
		}
		for _, slug := range []string{PermGroupsMembersSpawnCwd, PermGroupsMembersSpawnPrompts, PermGroupsMembersSpawnSandbox} {
			if holds(g.ID, slug) || holds(g.ID, PermGroupsMembersSpawnAll) {
				held = append(held, slug)
			}
		}
		out[g.Name] = held
	}
	return out
}

// projectPeerGroupSettings fills a group row with the settings held (the
// group's peerGroupGrants) lets the peer change, which the peer projection
// otherwise withholds: an editor that started blank would overwrite values
// the peer never saw. (Unrestricted trust reads the complete snapshot.)
func projectPeerGroupSettings(row *dashboardGroup, g *db.AgentGroup, peer string, held []string) {
	holds := func(slug string) bool { return slices.Contains(held, slug) }
	if holds(PermGroupsSettingsEnvironment) {
		row.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), g.Environment...)
	}
	if !holds(PermGroupsSettings) {
		return
	}
	attachment := groupAttachmentViewFor(g)
	row.AttachmentURL, row.AttachmentLabel, row.AttachmentLabelOverride = attachment.URL, attachment.Label, attachment.LabelOverride
	row.DefaultContext, row.ReinjectAfterCompact = g.DefaultContext, g.EffectiveReinjectAfterCompact()
	row.DefaultProfile = g.DefaultProfile
	row.MaxMembers, row.NotifyEnabled = g.MaxMembers, g.NotifyEnabled
	row.RemoteControlPolicy = remoteControlPolicyToWire(g.RemoteControl)
	if holds(PermGroupsMembersSpawnCwd) {
		row.DefaultCwd = g.DefaultCwd
	}
	if holds(PermGroupsMembersSpawnPrompts) {
		row.DefaultPrompts = append([]string(nil), g.DefaultPrompts...)
	}
	if holds(PermGroupsMembersSpawnSandbox) {
		row.SandboxProfile = g.SandboxProfile
	}
	// A parent the peer does not see stays unnamed.
	if g.ParentGroupID != nil && fedPeerGroupVisible(peer, *g.ParentGroupID) {
		if p, err := db.GetAgentGroupByID(*g.ParentGroupID); err == nil && p != nil && !p.IsArchived() {
			row.Parent = p.Name
		}
	}
}

// addPeerGroupSettingsRules declares the group controls a peer may run: the
// settings of a group's header and ⚙ menu (groups.settings), and its ⚙
// shut down and power on (the member lifecycle grants).
func addPeerGroupSettingsRules(rules map[string]peerViewRule) {
	settings := func(param string, extra func(string, *db.AgentGroup) []string) peerViewRule {
		return peerViewRule{feature: "groups.settings", requires: PermGroupsSettings, group: true, invoke: &peerInvokeSpec{param: param, extra: extra}}
	}
	for _, pattern := range []string{
		"PATCH /api/groups/{name}",
		"POST /api/groups/{name}/rename",
		"GET /api/groups/{name}/attachment",
		"POST /api/groups/{name}/attachment",
		"PUT /api/groups/{name}/parent",
	} {
		rules[pattern] = withPeerInvoke(pattern, settings("name", nil))
	}
	// Reading it is the spawn catalog's (node.spawn.read).
	for _, pattern := range []string{
		"PUT /api/groups/{group}/sandbox-profile",
		"DELETE /api/groups/{group}/sandbox-profile",
	} {
		rules[pattern] = withPeerInvoke(pattern, settings("group", peerGroupSandboxGates))
	}
	// A group's ⚙ shut down / power on: the member lifecycle grants, for
	// every member at once.
	for pattern, requires := range map[string]string{
		"POST /api/groups/{name}/shutdown": PermGroupsMembersStop,
		"POST /api/groups/{name}/power-on": PermGroupsMembersResume,
	} {
		feature := "lifecycle.stop"
		if requires == PermGroupsMembersResume {
			feature = "lifecycle.resume"
		}
		rules[pattern] = withPeerInvoke(pattern, peerViewRule{feature: feature, requires: requires, group: true, invoke: &peerInvokeSpec{param: "name"}})
	}
}

// withPeerInvoke serves a GET rule's reads and any other method's writes
// through servePeerInvoke.
func withPeerInvoke(pattern string, rule peerViewRule) peerViewRule {
	if strings.HasPrefix(pattern, "GET ") {
		rule.serve = servePeerInvoke
	} else {
		rule.write = servePeerInvoke
	}
	return rule
}
