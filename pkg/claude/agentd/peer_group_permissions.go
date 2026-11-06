package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A group's permission administration under peer grants: its owners
// (groups.owners.manage), its owner scopes (groups.settings.owner-scopes) and
// its member permission rows (groups.settings.member-permissions). Each
// confers authority on this node's agents, so what it would confer is held
// to the peer ceiling (peer_ceiling.go): never more than the peer itself
// holds here. Taking authority away (removing an owner, narrowing an owner
// scope, dropping a row) needs only the grant.

// peerReadsGroupPermissions reports whether peer is shown g's member
// permission rows: the grant that edits them. The PATCH that replaces the
// list takes the same, so a peer never replaces rows it was not shown.
func peerReadsGroupPermissions(peer string, g *db.AgentGroup) bool {
	return fedPeerAllows(peer, g.ID, PermGroupsSettingsMemberPermissions)
}

// peerReadsAnyGroupPermissions reports whether peer is shown the member
// permission rows of any of the groups in its view.
func peerReadsAnyGroupPermissions(peer string, groups []dashboardGroup) bool {
	for _, row := range groups {
		if g, err := db.GetAgentGroupByName(row.Name); err == nil && g != nil && peerReadsGroupPermissions(peer, g) {
			return true
		}
	}
	return false
}

// peerReadsGroupOwnerScopes is peerReadsGroupPermissions for owner scopes.
func peerReadsGroupOwnerScopes(peer string, g *db.AgentGroup) bool {
	return fedPeerAllows(peer, g.ID, PermGroupsSettingsOwnerScopes)
}

// peerOwnerAddFailure: an owner holds every owner-implied grant in the group
// (and its global bonuses), narrowed by the group's owner scopes only along
// dimensions, never slug by slug. So the peer must hold all of them. In
// practice that is unrestricted trust: ownership confers spawning and the
// member lifecycle, which no peer can confer (peerCeilingSlugs).
func peerOwnerAddFailure(v *peerView, _ *http.Request, g *db.AgentGroup, body []byte) *peerStructureFailure {
	if fail := peerHoldsInGroup(v, g, PermGroupsOwnersManage); fail != nil {
		return fail
	}
	var req struct {
		Conv string `json:"conv"`
	}
	if fail := peerStructureBody(body, &req, "conv"); fail != nil {
		return fail
	}
	conv, fail := peerAgentConv(v, req.Conv)
	if fail != nil {
		return fail
	}
	groups, err := db.ListGroupsForConv(conv)
	if err != nil {
		return peerStructureRefusal(http.StatusServiceUnavailable, "io", "read the group's members")
	}
	if !slices.ContainsFunc(groups, func(m *db.AgentGroup) bool { return m.ID == g.ID }) {
		return peerStructureRefusal(http.StatusForbidden, "permission", "a peer may make only a member of group:%s its owner", g.Name)
	}
	if missing := peerOwnerCeilingMissing(v.peer.InstanceID, g); len(missing) > 0 {
		return peerStructureRefusal(http.StatusForbidden, "permission",
			"an owner of group:%s holds what this node does not grant you: %s", g.Name, strings.Join(missing, "; "))
	}
	return nil
}

// peerOwnerCeilingMissing lists what an owner of g would hold beyond the
// peer's ceiling.
func peerOwnerCeilingMissing(peer string, g *db.AgentGroup) []string {
	var missing []string
	for _, slug := range OwnerImpliedSlugs() {
		var scope PermissionScope
		if slices.Contains(permissionScopeDimsForSlug(slug), ScopeDimGroup) {
			scope = PermissionScope{ScopeDimGroup: {g.Name}}
		}
		if err := peerCeilingCovers(peer, slug, scope); err != nil {
			missing = append(missing, err.Error())
		}
	}
	return missing
}

// peerOwnerRemoveFailure: removing an owner only takes authority away.
func peerOwnerRemoveFailure(v *peerView, _ *http.Request, g *db.AgentGroup, _ []byte) *peerStructureFailure {
	return peerHoldsInGroup(v, g, PermGroupsOwnersManage)
}

// peerGroupPermissionsFailure checks a peer's replacement of g's member
// permission rows. The peer must have been shown the current rows; every row
// it adds or changes must be within its ceiling, and a row it drops needs
// nothing more, nor does one whose new scope only narrows the stored one. A
// row given with no scope keeps the stored one
// (ReplaceAgentGroupPermissionGrants), so it is unchanged.
func peerGroupPermissionsFailure(peer string, g *db.AgentGroup, next []db.PermissionGrant) error {
	if !peerReadsGroupPermissions(peer, g) {
		return fmt.Errorf("replacing group:%s's permissions takes %s, which shows you the rows you would replace", g.Name, PermGroupsSettingsMemberPermissions)
	}
	rows, err := db.ListAgentGroupPermissionRows(g.ID)
	if err != nil {
		return fmt.Errorf("read the group's permissions: %w", err)
	}
	current := map[string]string{}
	for _, row := range rows {
		current[row.Slug] = row.ScopeJSON
	}
	for _, row := range next {
		scope := row.Scope
		stored, held := current[row.Slug]
		if scope == "" && !row.ScopeSpecified && held {
			scope = stored
		}
		if held && (scope == stored || peerRowNarrows(scope, stored)) {
			continue
		}
		if err := peerCeilingCoversGrant(peer, db.PermissionGrant{Slug: row.Slug, Scope: scope}); err != nil {
			return err
		}
	}
	return nil
}

// peerOwnerScopesFailure checks a peer's change of g's owner scopes. The
// peer must have been shown them. A change that only narrows what owners
// hold needs nothing more; a slug whose constraint it loosens or drops
// widens every owner's grant of it in the group, which the peer's ceiling
// must then cover.
func peerOwnerScopesFailure(peer string, g *db.AgentGroup, next string) error {
	if !peerReadsGroupOwnerScopes(peer, g) {
		return fmt.Errorf("changing group:%s's owner scopes takes %s, which shows you the current ones", g.Name, PermGroupsSettingsOwnerScopes)
	}
	cur, err := ownerScopesForEval(g.OwnerScopesJSON)
	if err != nil {
		return fmt.Errorf("group:%s's stored owner scopes cannot be read; only this node's operator can change them", g.Name)
	}
	nxt, err := ownerScopesForEval(next)
	if err != nil {
		return err
	}
	for slug, was := range cur {
		if now, ok := nxt[slug]; ok && peerScopeNarrows(now, was) {
			continue
		}
		var scope PermissionScope
		if slices.Contains(permissionScopeDimsForSlug(slug), ScopeDimGroup) {
			scope = PermissionScope{ScopeDimGroup: {g.Name}}
		}
		if err := peerCeilingCovers(peer, slug, scope); err != nil {
			return fmt.Errorf("loosening the owner scope of %s: %w", slug, err)
		}
	}
	return nil
}

// peerRowNarrows is peerScopeNarrows for a stored row's canonical scope JSON
// ("" for none). A scope that does not parse narrows nothing.
func peerRowNarrows(now, was string) bool {
	parse := func(raw string) (PermissionScope, bool) {
		if raw == "" {
			return PermissionScope{}, true
		}
		scope, _, err := parsePermissionScope(json.RawMessage(raw))
		return scope, err == nil
	}
	n, okNow := parse(now)
	w, okWas := parse(was)
	return okNow && okWas && peerScopeNarrows(n, w)
}

// peerScopeNarrows reports whether scope now permits no more than was: every
// dimension was constrains, now constrains to a subset of the same matchers
// (dimensions AND, matchers OR). A dimension now adds narrows further, except
// a peer dimension: a row with one authorizes remote actions in place of
// local ones (permissionScopeSatisfied), so adding it moves what the row
// reaches rather than narrowing it.
func peerScopeNarrows(now, was PermissionScope) bool {
	if len(now[ScopeDimPeer]) > 0 && len(was[ScopeDimPeer]) == 0 {
		return false
	}
	for dim, matchers := range was {
		got, ok := now[dim]
		if !ok || len(got) == 0 {
			return false
		}
		for _, m := range got {
			if !slices.Contains(matchers, m) {
				return false
			}
		}
	}
	return true
}

// projectPeerGroupPermissions fills a group row with the permission settings
// the peer edits there: its member permission rows, its owner scopes and
// which members own it, each under the grant that edits it, so an editor
// never starts blank over values the peer was not shown.
func projectPeerGroupPermissions(row *dashboardGroup, g *db.AgentGroup, held []string) error {
	if slices.Contains(held, PermGroupsSettingsMemberPermissions) {
		rows, err := db.ListAgentGroupPermissionRows(g.ID)
		if err != nil {
			return err
		}
		row.Permissions, row.PermissionScopes, row.UnreadablePermissionScopes = groupPermissionsView(rows)
	}
	if slices.Contains(held, PermGroupsSettingsOwnerScopes) {
		row.OwnerScopes = ownerScopesWire(g.OwnerScopesJSON)
	}
	if slices.Contains(held, PermGroupsOwnersManage) {
		owners, err := db.ListAgentGroupOwners(g.ID)
		if err != nil {
			return err
		}
		owned := map[string]bool{}
		for _, o := range owners {
			owned[o.ConvID] = true
		}
		for i := range row.Members {
			row.Members[i].Owner = owned[row.Members[i].ConvID]
		}
	}
	return nil
}

// groupPermissionsView is a group's member permission rows as the dashboard
// shows them: the slugs, the readable scopes, and the slugs whose stored
// scope this build cannot read.
func groupPermissionsView(rows []db.AgentGroupPermission) ([]string, map[string]PermissionScope, []string) {
	slugs, scopes, unreadable := []string{}, map[string]PermissionScope{}, []string{}
	for _, row := range rows {
		slugs = append(slugs, row.Slug)
		if scope := permissionScopeFromJSON(row.ScopeJSON); len(scope) > 0 {
			scopes[row.Slug] = scope
		} else if row.ScopeJSON != "" {
			unreadable = append(unreadable, row.Slug)
		}
	}
	return slugs, scopes, unreadable
}

// peerNameBindsAGrant reports whether a group permission row a peer wrote
// names group name in its group= scope. A scope names a group by name, and a
// rename keeps the name's grants where the name goes: a peer renaming or
// deleting such a group could move a row it wrote onto another group, past
// the ceiling it was judged by. An unreadable scope counts as naming it.
func peerNameBindsAGrant(name string) (bool, error) {
	scopes, err := db.ListPeerWrittenGroupPermissionScopes()
	if err != nil {
		return true, err
	}
	for _, raw := range scopes {
		scope := permissionScopeFromJSON(raw)
		if scope == nil || slices.Contains(scope[ScopeDimGroup], name) {
			return true, nil
		}
	}
	return false, nil
}

// peerRenameRebindsFailure refuses a peer's rename of a group from or to a
// name a peer-written permission names.
func peerRenameRebindsFailure(from, to string) error {
	for _, name := range []string{from, to} {
		if bound, err := peerNameBindsAGrant(name); err != nil || bound {
			return fmt.Errorf("a permission a peer conferred names group %q; renaming onto or away from that name would move it to another group, so only this node's operator may", name)
		}
	}
	return nil
}
