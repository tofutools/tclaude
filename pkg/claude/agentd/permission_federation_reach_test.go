package agentd

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Pins of the federation peer slug tables as they stood before they were
// derived from the registry's federation reach (permission_federation_reach.go).
// A change to what a peer can be granted, or to a slug's wire capability,
// must be deliberate: update the pin in the same change.

var pinnedFederationPeerSlugs = map[string]string{
	PermGroupsVisibilityRead:     proto.CapGroupVisibility,
	PermGroupsLinkAccept:         proto.CapGroupLinkAccept,
	PermAgentsStatusRead:         proto.CapAgentStatus,
	PermGroupsRosterRead:         proto.CapRoster,
	PermSessionsRead:             proto.CapSessions,
	PermSessionsWatch:            proto.CapSessionsWatch,
	PermSessionsAttach:           proto.CapSessionsAttach,
	PermSessionsTakeover:         proto.CapSessionsTakeover,
	PermSessionsFilesRead:        proto.CapSessionsFilesRead,
	PermSessionsShell:            proto.CapSessionsShell,
	PermGroupsPresenceRead:       proto.CapPresence,
	PermMessageDirect:            proto.CapMail,
	PermMessageAttachments:       proto.CapAttachments,
	PermAgentsReceive:            proto.CapAgentsReceive,
	PermAgentsReceivePermissions: proto.CapAgentsReceivePermissions,
	PermAgentsTeleportReceive:    proto.CapTeleportReceive,
	PermGroupsMembersSpawn:       proto.CapSpawn,
	// The launch settings a peer may choose on a spawn (federation_spawn_overrides.go).
	PermGroupsMembersSpawnCwd:         "spawn_cwd",
	PermGroupsMembersSpawnHarness:     "spawn_harness",
	PermGroupsMembersSpawnModel:       "spawn_model",
	PermGroupsMembersSpawnEffort:      "spawn_effort",
	PermGroupsMembersSpawnProfile:     "spawn_profile",
	PermGroupsMembersSpawnRoles:       "spawn_roles",
	PermGroupsMembersSpawnSandbox:     "spawn_sandbox",
	PermGroupsMembersSpawnPermissions: "spawn_permissions",
	PermGroupsMembersSpawnEnvironment: "spawn_environment",
	PermGroupsMembersSpawnOwner:       "spawn_owner",
	PermGroupsMembersSpawnFeatures:    "spawn_features",
	PermGroupsMembersSpawnPrompts:     "spawn_prompts",
	PermGroupsMembersSpawnWorktree:    "spawn_worktree",
	PermGroupsMembersSpawnAll:         "spawn_all",
	PermGroupsSettings:                "group_settings",
	PermGroupsSettingsEnvironment:     "group_environment",
	PermGroupsResourcesLock:           "resource_lock",
	PermGroupsResourcesRead:           "resource_read",
	PermGroupsMembersManage:           "member_manage",
	PermGroupsMembersStop:             "stop",
	PermGroupsMembersResume:           "resume",
	PermGroupsMembersRetire:           "retire",
	PermGroupsMembersClone:            "clone",
	PermAgentMove:                     "move",
	PermGroupsMembersInterrupt:        "interrupt",
	PermGroupsMembersCompact:          "compact",
	PermGroupsMembersReincarnate:      "reincarnate",
	PermGroupsMembersContextInfo:      "context_info",
	PermGroupsMembersPR:               "pr",
	PermGroupsMembersProcess:          "member_process",
	PermProcessRunsStart:              "process_runs_start",
	PermProcessAdvance:                "process_advance",
	PermGroupsMessagesSchedule:        "messages_schedule",
	PermGroupsMembersSchedule:         "member_schedule",
	PermGroupsTriggersRead:            "triggers_read",
	PermRoutesConsume:                 proto.CapRoutes,
	PermJobsRun:                       proto.CapJobs,
	// A fleet usage limit's actions on member agents (usage_fleet_receive.go).
	PermUsageRulesEnforce: "usage_enforce",
}

var pinnedFedInstancePeerSlugs = map[string]bool{
	"config.offer": true, PermApprovalsAnswer: true, PermNodeRead: true, PermFleetRead: true, PermProcessRunsRead: true, PermProcessRunsManage: true, PermNodeHarnessesRead: true,
	PermNodeUpdate: true, PermNodeHarnessesInstall: true, PermNodeCredentialsReceive: true, PermNodeExec: true,
	PermCostsRead: true, PermFederationAuditRead: true, PermNodeMessagesRead: true, PermNodeMessagesManage: true,
	PermNodeMessagesMarkRead: true, PermSnapshotsReceive: true, PermNodeSpawnRead: true, PermNodeSpawnDefaultsManage: true, PermNodeSpawnProfilesCreate: true, PermNodeGroupsCreate: true,
	// Usage sharing for fleet limits, and managing this node's usage rules.
	PermUsageRead: true, PermUsageRulesManage: true,
}

func TestFederationPeerSlugTablesArePinned(t *testing.T) {
	assert.Equal(t, pinnedFederationPeerSlugs, federationPeerSlugs, "group-scoped peer slugs and their wire capabilities")
	assert.Equal(t, pinnedFedInstancePeerSlugs, fedInstancePeerSlugs, "instance-wide peer slugs")
	// The http_proxy-scoped model gateway grants are the remaining peer slugs.
	for _, slug := range []string{PermModelsProxy, PermModelsProxyLeased} {
		_, group := federationPeerSlugs[slug]
		assert.False(t, group || fedInstancePeerSlugs[slug], "%s is neither a group nor an instance peer slug", slug)
	}
	meaningful, groupSlugs := peerAccessCatalog()
	assert.ElementsMatch(t, slices.Sorted(maps.Keys(pinnedFederationPeerSlugs)), groupSlugs)
	assert.Subset(t, meaningful, []string{"config.offer", PermSnapshotsReceive, PermApprovalsAnswer, PermModelsProxy, PermModelsProxyLeased})
}

// Every registered slug says what a federation peer can be granted for it,
// so a new slug cannot ship without that decision.
func TestEverySlugDeclaresFederationReach(t *testing.T) {
	kinds := []FedReachKind{FedPeerGroup, FedPeerInstance, FedPeerProxy, FedPeerAlias, FedSender, FedLocal}
	for _, entry := range permissionRegistry {
		reach, ok := slugFederationReach[entry.Slug]
		if !assert.True(t, ok, "%s declares no federation reach in slugFederationReach", entry.Slug) {
			continue
		}
		assert.Equal(t, reach, entry.Federation, "%s: the registry carries its reach", entry.Slug)
		assert.Contains(t, kinds, reach.Kind, entry.Slug)
		assert.Equal(t, reach.Kind == FedLocal, reach.Why != "", "%s: a local slug, and only one, says why it is never remote", entry.Slug)
		viaKinds := reach.Kind == FedPeerAlias || reach.Kind == FedSender
		assert.Equal(t, viaKinds, len(reach.Via) != 0, "%s: an alias or sender slug, and only one, names its peer grants", entry.Slug)
		if reach.Kind == FedLocal || reach.Kind == FedSender {
			assert.False(t, reach.Pending, "%s: only a peer grant is pending", entry.Slug)
		}
		if reach.Kind == FedPeerAlias || reach.Kind == FedSender || reach.Kind == FedLocal {
			assert.False(t, reach.Sensitive, "%s: sensitivity belongs to the peer grant it goes through", entry.Slug)
		}
	}
}

// An alias or sender slug goes through peer grants, and an alias is pending
// exactly while one of them is.
func TestFederationReachViaArePeerGrants(t *testing.T) {
	for slug, reach := range slugFederationReach {
		pending := false
		for _, via := range reach.Via {
			target := slugFederationReach[via]
			assert.Contains(t, []FedReachKind{FedPeerGroup, FedPeerInstance, FedPeerProxy}, target.Kind, "%s goes via %s, which is not a peer grant", slug, via)
			pending = pending || target.Pending
		}
		if reach.Kind == FedPeerAlias {
			assert.Equal(t, pending, reach.Pending, "%s: an alias is pending exactly while a grant it goes via is", slug)
		}
	}
}

// A global agent.* capability reaches a peer through its group sibling, the
// same mapping the local cross-agent gates use.
func TestFederationReachFollowsGroupSibling(t *testing.T) {
	for _, entry := range permissionRegistry {
		if entry.GroupSibling == "" {
			continue
		}
		sibling := slugFederationReach[entry.GroupSibling]
		if sibling.Kind == FedLocal {
			assert.Equal(t, FedLocal, entry.Federation.Kind, "%s: its group sibling %s never reaches a peer, so neither does it", entry.Slug, entry.GroupSibling)
			continue
		}
		want := []string{entry.GroupSibling}
		if sibling.Kind == FedPeerAlias {
			want = sibling.Via
		}
		assert.Equal(t, want, entry.Federation.Via, "%s reaches a peer as its group sibling %s does", entry.Slug, entry.GroupSibling)
	}
}

// An agent's peer= grant names a peer operation: the slug is a peer grant
// itself, or a sender slug the peer decides through its own grant.
func TestPeerScopedSlugsHaveFederationReach(t *testing.T) {
	for _, entry := range permissionRegistry {
		if !containsScopeDim(entry.ScopeDims, ScopeDimPeer) {
			continue
		}
		// An agent peer view route's slug is also a sender slug: the agent
		// holds it peer-scoped to use the route (peer_view_agents.go).
		assert.True(t, entry.Federation.peerGrantable() || entry.Federation.Kind == FedSender || slices.Contains(agentPeerViewSlugs(), entry.Slug),
			"%s accepts peer= scope but is neither a grantable peer slug nor a sender slug", entry.Slug)
	}
}

// A peer-invoked group route answers only group-local gates, and only
// through grantable group peer slugs.
func TestPeerGroupGateGrantsAreGroupLocal(t *testing.T) {
	for grant, gates := range peerGroupGateGrants {
		reach := slugFederationReach[grant]
		assert.True(t, reach.Kind == FedPeerGroup && reach.peerGrantable(), "%s answers group gates, so it is a grantable group peer slug", grant)
		for _, gate := range gates {
			assert.True(t, groupLocalGate(gate), "%s answers %s, which is not a group-local gate", grant, gate)
		}
	}
}

// A peer view route a grant opens requires a grantable peer slug; a route
// only unrestricted trust reaches names a slug a peer will be granted for it.
func TestPeerViewRulesMatchFederationReach(t *testing.T) {
	for pattern, rule := range peerViewRules() {
		if rule.publicRead && rule.requires == "" {
			continue
		}
		reach, known := slugFederationReach[rule.requires]
		if !known {
			assert.Contains(t, []string{"local_only", "peer_access"}, rule.requires, "%s requires unregistered %q", pattern, rule.requires)
			continue
		}
		if rule.unrestrictedOnly {
			assert.Contains(t, []FedReachKind{FedPeerGroup, FedPeerInstance}, reach.Kind, "%s: unrestricted-only route names %s, which is not a peer grant", pattern, rule.requires)
			continue
		}
		if rule.publicRead || rule.accessRequest {
			continue
		}
		assert.True(t, reach.peerGrantable(), "%s opens to peers holding %s, which is not a grantable peer slug", pattern, rule.requires)
	}
}

// The registry's risky slugs are sensitive wherever a peer can be granted them.
func TestRiskyPeerGrantsAreSensitive(t *testing.T) {
	for _, entry := range permissionRegistry {
		switch entry.Federation.Kind {
		case FedPeerGroup, FedPeerInstance, FedPeerProxy:
			if entry.Risky {
				assert.True(t, entry.Federation.Sensitive, "%s is risky, so as a peer grant it is sensitive", entry.Slug)
			}
		}
	}
}

// Peer bundle admission names grantable peer slugs.
func TestBundleAdmissionSlugsArePeerGrants(t *testing.T) {
	assert.Equal(t, PermConfigOffer, bundletransfer.Config.AdmissionSlug)
	for _, kind := range []bundletransfer.Type{bundletransfer.Agent, bundletransfer.Config, bundletransfer.Snapshot} {
		assert.True(t, slugFederationReach[kind.AdmissionSlug].peerGrantable(), "%s admits through %s", kind.Name, kind.AdmissionSlug)
	}
}

// peerSlugsMissingFromGrantPicker are grantable peer slugs the dashboard's
// add-grants picker does not list; each is granted elsewhere (the CLI, a
// group link or a move dialog) or not from the dashboard yet.
var peerSlugsMissingFromGrantPicker = []string{
	PermAgentsReceivePermissions, PermGroupsLinkAccept, PermGroupsVisibilityRead,
	PermSessionsFilesRead, PermSnapshotsReceive,
}

// PEER_SLUGS in the dashboard mirrors the peer slugs, their kind and their
// sensitive marker.
func TestDashboardPeerSlugsMatchFederationReach(t *testing.T) {
	raw, err := os.ReadFile("dashboard/js/fleet-admin-model.js")
	require.NoError(t, err)
	block := regexp.MustCompile(`(?s)export const PEER_SLUGS = Object\.freeze\(\[(.*?)\n\]\);`).FindSubmatch(raw)
	require.NotNil(t, block, "PEER_SLUGS not found")
	row := regexp.MustCompile(`(?m)^\s*\{ slug: '([^']+)', kind: '([a-z]+)'(.*)$`)
	listed := map[string]bool{}
	rows := row.FindAllStringSubmatch(string(block[1]), -1)
	require.Len(t, rows, strings.Count(string(block[1]), "slug: "), "every PEER_SLUGS row is one line in the { slug, kind, ... } form")
	for _, m := range rows {
		slug, kind, rest := m[1], m[2], m[3]
		listed[slug] = true
		reach := slugFederationReach[slug]
		if !assert.True(t, reach.peerGrantable(), "PEER_SLUGS lists %s, which is not a grantable peer slug", slug) {
			continue
		}
		if kind == "node" {
			assert.Contains(t, []FedReachKind{FedPeerInstance, FedPeerProxy}, reach.Kind, "%s: node kind", slug)
		} else {
			assert.Equal(t, FedPeerGroup, reach.Kind, "%s: %s kind", slug, kind)
		}
		assert.Equal(t, reach.Sensitive, strings.Contains(rest, "sensitive: true"), "%s: sensitive marker", slug)
	}
	for _, kind := range []FedReachKind{FedPeerGroup, FedPeerInstance, FedPeerProxy} {
		for _, slug := range fedPeerSlugsOfKind(kind) {
			assert.Equal(t, !slices.Contains(peerSlugsMissingFromGrantPicker, slug), listed[slug], "%s: listed in PEER_SLUGS unless a known exception", slug)
		}
	}
}

// The peer action pass-throughs name grantable peer slugs, and gates their
// own reach does not already answer through the same grant.
func TestPeerActionPassThroughsArePeerGrants(t *testing.T) {
	for gate, passes := range peerActionPassThroughs {
		_, ok := slugFederationReach[gate]
		assert.True(t, ok, "%s is a registered slug", gate)
		for _, p := range passes {
			assert.True(t, slugFederationReach[p.Via].peerGrantable(), "%s passes through %s, a grantable peer slug", gate, p.Via)
			if p.Footprint != "" {
				assert.True(t, slugFederationReach[p.Footprint].peerGrantable(), "%s footprint %s is a grantable peer slug", gate, p.Footprint)
			}
			assert.False(t, slices.Contains(peerGateGrants(gate), p.Via), "%s: %s already answers it by reach", gate, p.Via)
		}
	}
}
