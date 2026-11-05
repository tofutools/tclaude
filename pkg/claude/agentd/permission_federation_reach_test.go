package agentd

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

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
	PermRoutesConsume:                 proto.CapRoutes,
	PermJobsRun:                       proto.CapJobs,
	// A fleet usage limit's actions on member agents (usage_fleet_receive.go).
	PermUsageRulesEnforce: "usage_enforce",
}

var pinnedFedInstancePeerSlugs = map[string]bool{
	"config.offer": true, PermApprovalsAnswer: true, PermNodeRead: true, PermNodeHarnessesRead: true,
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
