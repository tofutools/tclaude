package agentd

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The peer ceiling: what a peer confers on this node (a permission row, an
// owner seat, an approval) never exceeds what the peer itself holds here.
//
// The dashboard handlers a peer's grant reaches run as this node's human, and
// on the peer path the granting caller reads as "" -- the human, whom the
// local attenuation (checkGrantAttenuation) leaves unconstrained. So every
// peer route that confers a permission checks each row it would write here
// first, and refuses the whole request when one is not covered.
//
// A local row is covered only when all of these hold:
//   - its slug is in peerCeilingSlugs: a local gate whose peer grant of the
//     same name reaches the same operation here, through a peer route that
//     adds no bound the local gate lacks;
//   - it names no peer: a row with a peer dimension authorizes actions on
//     other nodes (permissionScopeSatisfied), which no grant here measures,
//     while a row without one authorizes nothing remote;
//   - the peer holds the slug's peer grant in every group the row reaches:
//     the groups its group dimension names, or every group when it names
//     none (then the grant must be held unscoped). An instance-wide slug is
//     always held unscoped.
//
// Every other scope dimension only narrows a row (dimensions AND together),
// so it never widens what the ceiling must cover. Unrestricted trust holds
// every peer grant, and its ceiling is this node's operator's.
//
// Left out on purpose, so a row of them is never covered:
//   - federation-only grants with no local gate of their name (spawn
//     sub-grants, groups.settings, groups.members.manage, the node.* and
//     *.receive grants, approvals, config offers);
//   - grants whose peer form carries a policy a local row would drop
//     (groups.members.spawn and jobs.run launch policies, carry lists, the
//     visibility roster option);
//   - grants whose peer route bounds what the local gate does not: it refuses
//     options (stop's, resume's and restart's, retire's worktree deletion),
//     redacts (debug export), checks a footprint the local gate leaves out
//     (delete's owned and archived groups, add's and remove's other groups,
//     a link's other end, nesting's parent), takes extra grants (exports, clone, template
//     instantiation, process runs), withholds text a local reader sees
//     (trigger instruction templates), or sends no text into another agent's
//     context where the local verb may (compact's focus, reincarnate's
//     follow-up, cron jobs);
//   - grants whose local gate opens what the peer route keeps for another
//     grant: the group context's gate also sets default saved prompts
//     (spawn prompts for a peer), the resource lock's reaches resources in
//     no group, and the role library's lets a local agent attach grants it
//     holds to roles, which no peer may;
//   - global agent.* gates, which also reach agents in no group, where no
//     peer grant reaches; their group-scoped siblings are the grantable form;
//   - anything outside the reach registry or still pending.
var peerCeilingSlugs = map[string]bool{
	// An agent's controls, held in every group of the agent.
	PermGroupsMembersInterrupt:     true,
	PermGroupsMembersContextInfo:   true,
	PermGroupsMembersPR:            true,
	PermGroupsMembersRemoteControl: true,
	PermGroupsMembersRename:        true,
	PermGroupsMembersTags:          true,
	PermGroupsMembersTask:          true,
	PermGroupsMembersUpdate:        true,
	// A group's reads and its plain settings.
	PermGroupsRosterRead:                  true,
	PermGroupsPresenceRead:                true,
	PermGroupsResourcesRead:               true,
	PermGroupsSettingsEnvironment:         true,
	PermGroupsSettingsDescription:         true,
	PermGroupsSettingsMaxMembers:          true,
	PermGroupsSettingsNotifications:       true,
	PermGroupsSettingsRemoteControlPolicy: true,
	PermGroupsAttachment:                  true,
	PermGroupsRename:                      true,
	PermGroupsArchive:                     true,
	PermMessageAttachments:                true,
	PermProcessAdvance:                    true,
	// This node's libraries and its reads, instance-wide.
	PermPromptsManage:        true,
	PermCostsRead:            true,
	PermUsageRead:            true,
	PermProcessTemplatesRead: true,
}

// peerCeilingCovers reports whether peer holds everything a local permission
// row of slug under scope would confer: nil when it does, else an error
// naming what is missing.
func peerCeilingCovers(peer, slug string, scope PermissionScope) error {
	if db.FederationPeerUnrestricted(peer) {
		return nil
	}
	via, err := peerCeilingVia(slug)
	if err != nil {
		return err
	}
	if len(scope[ScopeDimPeer]) > 0 {
		return fmt.Errorf("%s scoped to another node stays this node's operator's", slug)
	}
	names := scope[ScopeDimGroup]
	if len(names) > 0 && !slices.Contains(permissionScopeDimsForSlug(slug), ScopeDimGroup) {
		return fmt.Errorf("%s takes no group scope", slug)
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return fmt.Errorf("read the peer's grants: %w", err)
	}
	for _, v := range via {
		if slugFederationReach[v].Kind == FedPeerInstance || len(names) == 0 {
			// Every group, or a grant no group scopes: held unscoped. A grant
			// that must name a group authorizes nothing unscoped.
			if fedPeerSlugRequiresGroup(v) || !slices.ContainsFunc(grants, func(g db.FederationPeerGrant) bool {
				return g.Slug == v && g.Scope == ""
			}) {
				if slugFederationReach[v].Kind != FedPeerInstance {
					return fmt.Errorf("%s with no group scope reaches every group: requires %s unscoped", slug, v)
				}
				return fmt.Errorf("%s requires %s", slug, v)
			}
			continue
		}
		for _, name := range names {
			g, err := db.GetAgentGroupByName(name)
			if err != nil || g == nil || g.IsArchived() {
				return fmt.Errorf("%s names group %q, which is not an active group here", slug, name)
			}
			if !fedPeerAllows(peer, g.ID, v) {
				return fmt.Errorf("%s requires %s in group:%s", slug, v, name)
			}
		}
	}
	return nil
}

// peerCeilingVia names the peer grants a local row of slug needs, or why
// none can cover it.
func peerCeilingVia(slug string) ([]string, error) {
	reach, ok := slugFederationReach[slug]
	if !ok || !peerCeilingSlugs[slug] {
		return nil, fmt.Errorf("%s cannot be conferred by a peer", slug)
	}
	var via []string
	switch reach.Kind {
	case FedPeerGroup, FedPeerInstance:
		via = []string{slug}
	case FedPeerAlias:
		via = reach.Via
	}
	if len(via) == 0 {
		return nil, fmt.Errorf("%s cannot be conferred by a peer", slug)
	}
	for _, v := range via {
		// A pending grant no peer holds yet; an alias's own Pending flag
		// is its Via's.
		r, ok := slugFederationReach[v]
		if !ok || r.Pending || (r.Kind != FedPeerGroup && r.Kind != FedPeerInstance) {
			return nil, fmt.Errorf("%s cannot be conferred by a peer", slug)
		}
	}
	return via, nil
}

// peerCeilingCoversGrant is peerCeilingCovers for a stored row, whose scope
// is canonical scope JSON ("" for none). A scope that does not parse is not
// covered.
func peerCeilingCoversGrant(peer string, grant db.PermissionGrant) error {
	var scope PermissionScope
	if grant.Scope != "" {
		parsed, _, err := parsePermissionScope(json.RawMessage(grant.Scope))
		if err != nil {
			return fmt.Errorf("%s: unreadable scope: %w", grant.Slug, err)
		}
		scope = parsed
	}
	return peerCeilingCovers(peer, grant.Slug, scope)
}
