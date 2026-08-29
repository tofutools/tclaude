package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// PermSlug describes one agent permission. Slugs are dotted strings that
// identify capabilities the daemon evaluates via requirePermission.
//
// Defaults — granted to every agent — live in ~/.tclaude/config.json
// under agent.default_permissions. Per-conv grants live in SQLite (table
// agent_permissions) and are written through the grant/revoke endpoints.
//
// OwnerImplied marks a slug group ownership contributes as an ordinary
// permission source. If the slug declares ScopeDimGroup, each owned active
// group contributes one grant scoped to that group; otherwise owning any
// active group contributes an unscoped global owner bonus. Per-group owner
// constraints add dimensions to that derived grant. An explicit per-conv deny
// still suppresses the owner tier.
// AutoGrantable marks a slug the approval popup may persist from its
// "Always allow for this agent" button — a one-click write of an allow
// override alongside approving the pending request, so future calls skip
// the popup. It is a deliberately SMALL allowlist: only low-blast-radius,
// human-machine-surface slugs qualify (the clipboard / notify channels).
// Destructive or fleet-affecting slugs (agent.delete, groups.delete, the
// permissions.* meta-slugs) are NOT auto-grantable — persisting those from
// a single popup click is too sharp an edge; the human sets them
// deliberately via the permission editor / config. Rendered button
// visibility AND the popup's server-side persist both gate on this flag,
// so a scraped popup URL can't self-grant an ineligible slug. Kept in
// lockstep with the popup by TestPermissionRegistry_AutoGrantableSet.
type PermSlug struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	// ScopeDims declares the dimensions a grant for this slug may constrain.
	// An empty list means the slug only supports today's unscoped grants.
	ScopeDims    []ScopeDim `json:"scope_dims,omitempty"`
	OwnerImplied bool       `json:"owner_implied,omitempty"`
	// GroupSibling is the group-scoped alternative for a global agent.*
	// capability. Cross-agent gates evaluate the global slug first, then require
	// complete current-group coverage through this sibling.
	GroupSibling string `json:"group_sibling,omitempty"`
	// MemberImplied marks a group-scoped slug conferred by membership in the
	// action's target group. Like ownership, it is a structural source below an
	// explicit deny, not an endpoint-specific bypass.
	MemberImplied bool `json:"member_implied,omitempty"`
	AutoGrantable bool `json:"auto_grantable,omitempty"`
}

func initPermissionRegistry() {
	initPermissionRegistryEntries(permissionRegistry)
}

func initPermissionRegistryEntries(registry []PermSlug) {
	seenSlugs := map[string]bool{}
	for i := range registry {
		p := &registry[i]
		if p.Slug == "" {
			panic("permission registry: empty slug")
		}
		if seenSlugs[p.Slug] {
			panic(fmt.Sprintf("permission registry: duplicate slug %q", p.Slug))
		}
		seenSlugs[p.Slug] = true
		seenDims := map[ScopeDim]bool{}
		for _, dim := range p.ScopeDims {
			if _, ok := permissionScopeDimensions[dim]; !ok {
				panic(fmt.Sprintf("permission registry: slug %q declares unknown scope dimension %q", p.Slug, dim))
			}
			if seenDims[dim] {
				panic(fmt.Sprintf("permission registry: slug %q declares scope dimension %q more than once", p.Slug, dim))
			}
			seenDims[dim] = true
		}
		if p.OwnerImplied && strings.HasPrefix(p.Slug, "groups.") &&
			!seenDims[ScopeDimGroup] {
			panic(fmt.Sprintf("permission registry: owner-conferred group slug %q must declare scope dimension %q",
				p.Slug, ScopeDimGroup))
		}
	}
	for _, p := range registry {
		if p.GroupSibling == "" {
			continue
		}
		var sibling *PermSlug
		for i := range registry {
			if registry[i].Slug == p.GroupSibling {
				sibling = &registry[i]
				break
			}
		}
		if sibling == nil || !sibling.OwnerImplied || !containsScopeDim(sibling.ScopeDims, ScopeDimGroup) {
			panic(fmt.Sprintf("permission registry: slug %q has invalid group sibling %q", p.Slug, p.GroupSibling))
		}
	}
}

func containsScopeDim(dims []ScopeDim, want ScopeDim) bool {
	for _, dim := range dims {
		if dim == want {
			return true
		}
	}
	return false
}

func init() { initPermissionRegistry() }

// permissionRegistry is the single source of truth for known slugs. It's
// what `permissions slugs` returns and what the validators consult when
// they want to refuse an unknown slug. Legacy config/DB rows may retain an
// unknown string for round-trip compatibility, but it is inert until a future
// build registers it; all mutation and authorization boundaries accept only
// this vocabulary.
var permissionRegistry = []PermSlug{
	{
		Slug:        PermSelfRename,
		Description: "Rename own conversation via /rename (tclaude agent rename)",
	},
	{
		Slug:        PermSelfCompact,
		Description: "Compact own conversation via /compact (tclaude agent compact)",
	},
	{
		Slug:        PermSelfInterrupt,
		Description: "Interrupt own active Codex app-server turn (tclaude agent interrupt)",
	},
	{
		Slug:        PermSelfClone,
		Description: "Fork this agent into a sibling that inherits its identity; the original keeps running (tclaude agent clone)",
	},
	{
		Slug:        PermSelfRemoteControl,
		Description: "Toggle own Claude Code Remote Access (tclaude agent remote-control). Default-granted for supported harnesses.",
	},
	{
		Slug:        PermSelfTask,
		Description: "Set/clear own task-reference link — the Task column's Linear/GitHub/ticket URL (tclaude agent task set/clear). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:        PermSelfPR,
		Description: "Present own pull request to the operator dashboard (tclaude agent present-pr). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:        PermSelfTags,
		Description: "Set own agent tags — the short labels rendered as chips in the Description column (tclaude agent tags set/add/rm). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug:        PermSelfDirRepair,
		Description: "Recreate own recorded startup directory when it has been deleted (tclaude agent dir --repair). The path is daemon-selected and cannot be overridden. Default-granted.",
	},
	{
		Slug: PermAgentReincarnate, GroupSibling: PermGroupsMembersReincarnate,
		Description: "Reincarnate ANOTHER agent globally (tclaude agent reincarnate --target). Group-scoped authority uses groups.members.reincarnate.",
	},
	{
		Slug: PermAgentCompact, GroupSibling: PermGroupsMembersCompact,
		Description: "Compact ANOTHER agent's context window globally (tclaude agent compact --target). Group-scoped authority uses groups.members.compact.",
	},
	{
		Slug: PermAgentInterrupt, GroupSibling: PermGroupsMembersInterrupt,
		Description: "Interrupt ANOTHER agent's active Codex app-server turn globally (tclaude agent interrupt --target). Group-scoped authority uses groups.members.interrupt.",
	},
	{
		Slug: PermAgentRename, GroupSibling: PermGroupsMembersRename,
		Description: "Rename ANOTHER agent globally (tclaude agent rename --target). Group-scoped authority uses groups.members.rename.",
	},
	{
		Slug: PermAgentClone, GroupSibling: PermGroupsMembersClone,
		Description: "Clone ANOTHER agent globally into a sibling that inherits its identity (tclaude agent clone --target). Group-scoped authority uses groups.members.clone.",
	},
	{
		Slug: PermAgentContextInfo, GroupSibling: PermGroupsMembersContextInfo,
		Description: "Read ANOTHER agent's context-window state globally (tclaude agent context-info --target / --group). Group-scoped authority uses groups.members.context-info.",
	},
	{
		Slug: PermAgentDebugExport, GroupSibling: PermGroupsMembersDebugExport,
		Description: "Export ANOTHER agent's recorded launch and sandbox configuration globally (tclaude agent debug-export <target>). Group-scoped authority uses groups.members.debug-export.",
	},
	{
		Slug: PermAgentTask, GroupSibling: PermGroupsMembersTask,
		Description: "Set/clear ANOTHER agent's task-reference link globally (tclaude agent task set/clear --target). Group-scoped authority uses groups.members.task.",
	},
	{
		Slug: PermAgentPR, GroupSibling: PermGroupsMembersPR,
		Description: "Present or handle ANOTHER agent's pull request globally (tclaude agent present-pr --target). Group-scoped authority uses groups.members.pr.",
	},
	{
		Slug: PermAgentTags, GroupSibling: PermGroupsMembersTags,
		Description: "Set ANOTHER agent's tags globally (tclaude agent tags set/add/rm --target). Group-scoped authority uses groups.members.tags.",
	},
	{
		Slug: PermAgentSpawn, GroupSibling: PermGroupsMembersSpawn,
		ScopeDims:   []ScopeDim{ScopeDimGroup, ScopeDimSpawnProfile, ScopeDimSandboxProfile},
		Description: "Spawn a fresh agent into any group globally. Group-scoped authority uses groups.members.spawn.",
	},
	{
		Slug: PermGroupsMembersReincarnate, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Reincarnate another agent when all of its current active group memberships are covered.",
	},
	{
		Slug: PermGroupsMembersCompact, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Compact another agent when all of its current active group memberships are covered.",
	},
	{
		Slug: PermGroupsMembersInterrupt, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Interrupt another agent when all of its current active group memberships are covered.",
	},
	{
		Slug: PermGroupsMembersRename, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Rename another agent when all of its current active group memberships are covered.",
	},
	{
		Slug: PermGroupsMembersClone, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Clone another agent when all current source and inherited destination groups are covered.",
	},
	{
		Slug: PermGroupsMembersContextInfo, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Read another member's context-window state when its current active groups are covered.",
	},
	{
		Slug: PermGroupsMembersDebugExport, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Export another member's recorded launch and sandbox configuration when its current active groups are covered.",
	},
	{
		Slug: PermGroupsMembersTask, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Set another member's task-reference link when its current active groups are covered.",
	},
	{
		Slug: PermGroupsMembersPR, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Present another member's pull request when its current active groups are covered.",
	},
	{
		Slug: PermGroupsMembersTags, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Set another member's tags when its current active groups are covered.",
	},
	{
		Slug:        PermGroupsAdmin,
		Description: "Umbrella for every registered groups.* group-administration operation. A dedicated per-operation deny still wins, so groups.admin can be narrowed without replacing it with many grants.",
	},
	{
		Slug:        PermGroupsCreate,
		Description: "Create new agent groups (tclaude agent groups create)",
	},
	{
		Slug:        PermGroupsDelete,
		Description: "Delete agent groups (tclaude agent groups rm)",
	},
	{
		Slug:         PermGroupsMembersStop,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Stop a group's running members (tclaude agent groups stop), or one agent when every affected current group is covered. Ownership contributes this slug scoped to each owned group.",
	},
	{
		Slug:         PermGroupsMembersResume,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Resume a group's offline members (tclaude agent groups resume), or one agent when every affected current group is covered. Ownership contributes this slug scoped to each owned group.",
	},
	{
		Slug:         PermGroupsMembersRetire,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup, ScopeDimTargetAgent},
		Description:  "Retire members in bulk or retire one agent when every affected current group is covered. Demotes each member to a plain conversation, drops memberships, and revokes permission/sudo grants. Ownership contributes this slug scoped to each owned group.",
	},
	{
		Slug:         PermGroupsMembersSpawn,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup, ScopeDimSpawnProfile, ScopeDimSandboxProfile},
		Description:  "Spawn a fresh session and add it to a group (tclaude agent spawn). Ownership contributes this slug scoped to each owned group; spawn guardrails still apply.",
	},
	{
		Slug:         PermGroupsOwnersManage,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Grant or revoke group ownership (tclaude agent groups grant-owner / revoke-owner). Ownership contributes this slug scoped to each owned group.",
	},
	{
		Slug:         PermGroupsRename,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Rename a group (tclaude agent groups rename). Ownership contributes this slug scoped to each owned group. This permission authorizes only rename.",
	},
	{
		Slug:         PermGroupsSettingsDescription,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's description (tclaude agent groups set-descr)",
	},
	{
		Slug:         PermGroupsSettingsDefaultDir,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's default spawn directory (tclaude agent groups set-default-dir)",
	},
	{
		Slug:         PermGroupsSettingsDefaultContext,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's shared startup context (tclaude agent groups set-context)",
	},
	{
		Slug:         PermGroupsSettingsEnvironment,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's common spawn environment",
	},
	{
		Slug:        PermGroupsSettingsDefaultSpawnTarget,
		Description: "Make or unmake a group the default spawn target (tclaude agent groups set-default)",
	},
	{
		Slug:         PermGroupsSettingsDefaultProfile,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's default spawn profile (tclaude agent groups set-default-profile)",
	},
	{
		Slug:         PermGroupsSettingsMaxMembers,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's hard member cap (tclaude agent groups set-max-members)",
	},
	{
		Slug:         PermGroupsSettingsNotifications,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Mute or unmute notifications for a group's agents (tclaude agent groups set-notifications)",
	},
	{
		Slug:         PermGroupsSettingsRemoteControlPolicy,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's Claude Code remote-control policy (tclaude agent groups set-remote-control)",
	},
	{
		Slug:         PermGroupsSettingsMemberPermissions,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Replace a group's live member permission grants. Also requires permissions.grant and permissions.revoke.",
	},
	{
		Slug:         PermGroupsSettingsOwnerScopes,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's owner-grant constraints. Also requires permissions.grant and permissions.revoke.",
	},
	{
		Slug:         PermGroupsMembersAdd,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Add members to a group (tclaude agent groups add)",
	},
	{
		Slug:         PermGroupsMembersRemove,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Remove members from a group (tclaude agent groups remove)",
	},
	{
		Slug:         PermGroupsMembersUpdate,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Edit role/descr on existing members (tclaude agent groups update-member)",
	},
	{
		Slug:          PermGroupsMessagesSchedule,
		OwnerImplied:  true,
		ScopeDims:     []ScopeDim{ScopeDimGroup},
		MemberImplied: true,
		Description:   "Create and manage recurring messages targeting a group. Membership or ownership confers this slug only for that group.",
	},
	{
		Slug:        PermPermissionsGrant,
		Description: "Grant agent permissions (tclaude agent permissions grant)",
	},
	{
		Slug:        PermPermissionsRevoke,
		Description: "Revoke agent permissions (tclaude agent permissions revoke)",
	},
	{
		Slug:        PermSelfSchedule,
		Description: "Manage own scheduled cron jobs — list / add / remove (tclaude agent cron). Default-granted, mirroring the self-lifecycle slugs.",
	},
	{
		Slug: PermAgentSchedule, GroupSibling: PermGroupsMembersSchedule,
		Description: "Manage ANOTHER agent's scheduled cron jobs globally. Group-scoped authority uses groups.members.schedule.",
	},
	{
		Slug: PermGroupsMembersSchedule, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Manage another member's scheduled cron jobs when its current active groups are covered.",
	},
	{
		Slug: PermAgentStop, GroupSibling: PermGroupsMembersStop,
		Description: "Stop ANOTHER agent's tmux session globally. Group-scoped authority uses groups.members.stop.",
	},
	{
		Slug: PermAgentResume, GroupSibling: PermGroupsMembersResume,
		Description: "Resume ANOTHER agent globally into a fresh tmux session. Group-scoped authority uses groups.members.resume.",
	},
	{
		Slug:         PermGroupsArchive,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Archive (soft-delete) a group: freezes membership + ownership and hides the group from default listings, while preserving message history (tclaude agent groups archive / unarchive)",
	},
	{
		Slug:         PermGroupsNest,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Nest a group under another as a subgroup, or clear its parent. Board organisation only. Ownership contributes this slug scoped to each owned group.",
	},
	{
		Slug:         PermGroupsAttachment,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Set or clear a group's persistent external reference link. Ownership contributes this slug scoped to each owned group.",
	},
	{
		Slug:        PermGroupsClone,
		Description: "Clone a group and its selected group-level configuration (tclaude agent groups clone)",
	},
	{
		Slug: PermAgentDelete, GroupSibling: PermGroupsMembersDelete,
		Description: "Permanently delete ANOTHER agent globally. Group-scoped authority uses groups.members.delete.",
	},
	{
		Slug: PermGroupsMembersDelete, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Permanently delete another member when all current active groups containing it are covered.",
	},
	{
		Slug:         PermGroupsLinkAdd,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Create an inter-group link enabling messages from one group to another. Ownership contributes this slug scoped to each owned source group.",
	},
	{
		Slug:         PermGroupsLinkRemove,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Remove an inter-group link. Ownership contributes this slug scoped to each owned source group.",
	},
	{
		Slug: PermAgentPromote, GroupSibling: PermGroupsMembersPromote,
		Description: "Promote a plain conversation into an agent, or reinstate a retired one, globally. Group-scoped authority uses groups.members.promote.",
	},
	{
		Slug: PermGroupsMembersPromote, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Promote or reinstate another member when its current active groups are covered.",
	},
	{
		Slug: PermAgentRetire, GroupSibling: PermGroupsMembersRetire,
		ScopeDims:   []ScopeDim{ScopeDimGroup, ScopeDimTargetAgent},
		Description: "Retire an agent globally. Group-scoped authority uses groups.members.retire.",
	},
	{
		Slug:        PermAgentStanddown,
		ScopeDims:   []ScopeDim{ScopeDimGroup, ScopeDimTargetAgent},
		Description: "Stand down an agent from active work. Reserved for the scoped-permissions standdown flow.",
	},
	{
		Slug: PermAgentRemoteControl, GroupSibling: PermGroupsMembersRemoteControl,
		Description: "Toggle ANOTHER agent's built-in remote access (tclaude agent remote-control --target). " +
			"Group-scoped authority uses groups.members.remote-control.",
	},
	{
		Slug: PermGroupsMembersRemoteControl, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Toggle another member's remote access when its current active groups are covered.",
	},
	{
		Slug: PermAgentSandboxImplementation,
		Description: "Assign the durable sandbox implementation an EXISTING offline agent relaunches under (tclaude agent sandbox-impl set). " +
			"It can move an agent onto an implementation with no OS-level access confinement, so group ownership does NOT confer it and it is not default-granted (effectively human-only).",
	},
	{
		Slug: PermAgentInboxWatch, GroupSibling: PermGroupsMembersInboxWatch,
		Description: "Watch ANOTHER agent's inbox — a live read of messages addressed to it. " +
			"Group-scoped authority uses groups.members.inbox-watch.",
	},
	{
		Slug: PermGroupsMembersInboxWatch, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Watch another member's inbox when its current active groups are covered.",
	},
	{
		Slug:        PermMessageDirect,
		Description: "Send a 1:1 message to ANY agent regardless of shared-group membership — the off-group escape hatch (tclaude agent message). Intra-group messaging, owner-of-group, and via-link reach need no slug; this covers everything else. Not default-granted.",
	},
	{
		Slug:        PermGroupsExport,
		Description: "Export a whole group to a portable .zip archive — DB rows plus every member's conversation .jsonl (tclaude agent groups export). The archive holds full conversation content; not default-granted (effectively human-only).",
	},
	{
		Slug:        PermGroupsImport,
		Description: "Import a group from a .zip archive, recreating the group, its agents, permissions and conversations on this machine (tclaude agent groups import). Not default-granted (effectively human-only).",
	},
	{
		Slug:        PermTemplatesManage,
		Description: "Create, edit, delete group templates and snapshot a live group into a template (dashboard Templates tab). A template is a reusable blueprint, not a conversation snapshot. Not default-granted (effectively human-only).",
	},
	{
		Slug:         PermTemplatesUse,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Instantiate a working group from a template, or rebrief/reinforce an existing group. Ownership contributes this slug scoped to each owned group; instantiating a new group still needs another positive source.",
	},
	{
		Slug:        PermProfilesManage,
		Description: "Create, edit and delete reusable spawn profiles — named, saved bundles of the spawn-agent dialog (harness/model/effort/role/… ) that pre-fill spawns and back a group's default spawn settings (JOH-210). Reads are open; writes rewrite shared spawn config, so not default-granted (effectively human-only).",
	},
	{
		Slug: PermSandboxHarnessConfig,
		Description: "Select the harness-config posture for a spawn (--harness-config), i.e. ask for the read-only floor over the harness's own settings, hook, skill and command surface to be lifted. " +
			"Lifting it lets the launched agent rewrite the policy that confines it and drop code that runs in the human's next unsandboxed session, so group ownership does NOT confer it and it is not default-granted (effectively human-only). " +
			"It gates selection only; sandbox lineage still refuses a child whose posture is wider than its recorded parent.",
	},
	{
		Slug:        PermSandboxProfilesManage,
		Description: "Read, create, edit, delete and assign sandbox profiles — operator policy that can add host filesystem access and launch environment to agents. This is intentionally separate from profiles.manage and is not default-granted (effectively human-only).",
	},
	{
		Slug:        PermSandboxProfilesDraft,
		Description: "Submit a server-validated sandbox-profile draft for explicit human preview and save. Cannot persist or assign profiles and cannot launch agents. Intended for dashboard-summoned sandbox scribes.",
	},
	{
		Slug:        PermRolesManage,
		Description: "Create, edit and delete roles in the role library — named, reusable bundles of a canonical role-brief and default permission grants. Launch policy belongs to spawn profiles. Reads are open; writes rewrite shared role defaults, so not default-granted (effectively human-only).",
	},
	{
		Slug:         PermProcessAdvance,
		OwnerImplied: true,
		ScopeDims:    []ScopeDim{ScopeDimGroup},
		Description:  "Advance a group's advisory process to the next or a named phase. Ownership contributes this slug scoped to each owned group. Reads are open.",
	},
	{
		Slug:        PermProcessTemplatesRead,
		Description: "List, show, and validate process templates through tclaude agent process-templates. Read-only and installed as a default alongside the bundled process-template scribe skill; an explicit per-agent deny still wins.",
	},
	{
		Slug:        PermProcessTemplatesManage,
		Description: "Create and edit process templates through tclaude agent process-templates save. Does not execute or instantiate a process. Not default-granted; requires an explicit grant or one-shot human approval.",
	},
	{
		Slug:         PermProcessRunsRead,
		OwnerImplied: true,
		Description:  "List and inspect daemon-owned process runs and reconciliation state. Group owners get this by default (a coordinating role driving process validation needs run status and evidence without a popup per read), suppressible by a deny override; otherwise not in the global defaults, because runtime state can contain bound command details and parameters. Read-only — it confers no run mutation authority.",
	},
	{
		Slug:        PermProcessRunsManage,
		ScopeDims:   []ScopeDim{ScopeDimProcessTemplate},
		Description: "Create, resume, and explicitly reconcile daemon-owned process runs, including executing the run's persisted authorized program profiles. Not default-granted; requires an explicit grant or one-shot human approval.",
	},
	{
		Slug: PermTriggersRead, GroupSibling: PermGroupsTriggersRead,
		Description: "Read global trigger rules and their firing ledger. Group-scoped authority uses groups.triggers.read.",
	},
	{
		Slug: PermTriggersManage, GroupSibling: PermGroupsTriggersManage,
		Description: "Create and mutate global trigger rules. Group-scoped authority uses groups.triggers.manage.",
	},
	{
		Slug: PermGroupsTriggersRead, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Read trigger rules and firing history for an owned group.",
	},
	{
		Slug: PermGroupsTriggersManage, OwnerImplied: true, ScopeDims: []ScopeDim{ScopeDimGroup},
		Description: "Create and mutate trigger rules for an owned group.",
	},
	{
		Slug:          PermHumanNotify,
		OwnerImplied:  true,
		AutoGrantable: true,
		Description:   "Send the human a notification via `tclaude agent notify-human` — it lands in the dashboard Messages tab. Lets a coordinating agent (the PO) reach the human outside the terminal. Group owners get this by default (a trusted coordinating role), suppressible by a deny override; otherwise not in the global defaults, so plain workers cannot spam the channel without an explicit grant.",
	},
	{
		Slug:          PermHumanClipboard,
		AutoGrantable: true,
		Description:   "Copy text to the human's system clipboard via `tclaude agent clipboard` — the daemon runs the platform copy tool (wl-copy/xclip/xsel, pbcopy, clip.exe). An agent→human-machine surface like human.notify, but NOT default-granted and NOT owner-implied: it writes to the operator's real clipboard, so it needs an explicit grant or a per-call --ask-human popup approval.",
	},
	{
		Slug:        session.PermAutoPermitEnterWorktree,
		Description: "Let tclaude answer Claude Code's EnterWorktree safety check for this agent — the hardcoded confirmation that no allow-rule, auto-mode setting or PreToolUse hook can pre-approve, so an agent otherwise stalls on it until a human presses a key. The grant IS the operator's standing consent: the PermissionRequest hook presses the accept key in the agent's own pane and records the answer. NOT default-granted and NOT owner-implied — it answers a gate the harness deliberately reserves for a human. Narrow by construction: one slug per named prompt, never a blanket accept (that is what --dangerously-skip-permissions is for).",
	},
	{
		Slug:        PermSettingsDefaultModel,
		Description: "Set or clear the user-level default Claude model — the \"model\" key in ~/.claude/settings.json, which every claude launched without --model falls back to. Rewrites a config file in the human's home, so not default-granted (effectively human-only).",
	},
	{
		Slug:        PermRoutesPublish,
		ScopeDims:   []ScopeDim{ScopeDimGroup},
		Description: "Register and withdraw routes owned by the caller. Requires current membership in the explicitly selected target group; not globally default-granted.",
	},
	{
		Slug:        PermRoutesConsume,
		ScopeDims:   []ScopeDim{ScopeDimGroup},
		Description: "Open/lease a published route and close the caller's own lease. Requires current membership in the explicitly selected target group; not globally default-granted.",
	},
	{
		Slug:      PermGitRead,
		ScopeDims: []ScopeDim{ScopeDimRemote},
		Description: "Read from a Git remote through the daemon — list remotes, ls-remote, fetch (tclaude proxy git). " +
			"agentd runs git on the host with ITS OWN credentials, so a sandboxed agent that cannot read ~/.ssh can still " +
			"sync with the remote. Bounded by the operator's agent.git_proxy.allowed_remotes list and by the agent's own " +
			"recorded launch repository. Not default-granted and not owner-implied: it spends the operator's credential.",
	},
	{
		Slug:      PermGitPush,
		ScopeDims: []ScopeDim{ScopeDimRemote},
		Description: "Push to a Git remote through the daemon (tclaude proxy git push). Strictly more powerful than proxy.git.read — " +
			"it writes to the forge as the operator. Refuses operator-protected branches (agent.git_proxy.protected_refs) " +
			"outright, and force-with-lease only when agent.git_proxy.allow_force_push is on. Not default-granted and not " +
			"owner-implied.",
	},
	{
		Slug:      PermGitHubRead,
		ScopeDims: []ScopeDim{ScopeDimRemote},
		Description: "Read GitHub pull requests, issues and CI results through the daemon's gh credentials (tclaude proxy github " +
			"pr ls/view/checks/comments, issue ls/view, run ls/log-failed/artifacts/download). Restricted to the repository the " +
			"agent's own remote resolves to, and only when that remote is on the operator's allow-list. Not default-granted: it " +
			"reads private repository data as the operator. Note that run download also WRITES: it unpacks a run's artifacts " +
			"into .tclaude-artifacts/ in the agent's own work tree. It cannot write anywhere else — the destination is " +
			"computed, never requested — and it cannot accumulate: 512 MiB compressed and 2 GiB unpacked per download, at " +
			"most 3 run directories kept, each run's directory emptied before it is refilled.",
	},
	{
		Slug:      PermGitHubWrite,
		ScopeDims: []ScopeDim{ScopeDimRemote},
		Description: "Create and comment on GitHub pull requests and issues through the daemon's gh credentials " +
			"(tclaude proxy github pr create/edit/comment/ready, issue comment). Everything it writes is attributed to the " +
			"operator's GitHub account, so it is not default-granted and not owner-implied. It does NOT confer " +
			"proxy.github.merge.",
	},
	{
		Slug:      PermGitHubMerge,
		ScopeDims: []ScopeDim{ScopeDimRemote},
		Description: "Merge a GitHub pull request through the daemon's gh credentials (tclaude proxy github pr merge). " +
			"Split off from proxy.github.write, and not implied by it: opening a pull request proposes a change, while merging " +
			"one lands it on the base branch under the operator's GitHub account. GitHub's own branch protection and required " +
			"checks still apply. agent.git_proxy.protected_refs does NOT, because that list bounds direct pushes, while a " +
			"pull request may target any branch — ordinarily one of that list's defaults (main, master), which is what " +
			"would make honouring it here a no-op for most of the merges a grant is given for. Not default-granted and " +
			"not owner-implied.",
	},
	{
		Slug:      PermLinearRead,
		ScopeDims: []ScopeDim{ScopeDimLinearTeam},
		Description: "Read Linear issues and comments through the daemon's Linear API key (tclaude proxy linear whoami, " +
			"issue view/ls/comments/search). Narrowable per agent with --scope linear_team=TCL: with an operator " +
			"agent.linear_proxy.allowed_teams list configured the two intersect and the scope can only narrow it, while with " +
			"no such list a scoped grant is the whole team policy. An UNSCOPED grant is refused outright when the operator " +
			"has no list. Not default-granted: it reads private workspace data as the operator.",
	},
	{
		Slug:      PermLinearWrite,
		ScopeDims: []ScopeDim{ScopeDimLinearTeam},
		Description: "Create and update Linear issues, comment on them, and attach links, through the daemon's Linear API key " +
			"(tclaude proxy linear issue create/comment/update/link). Everything it writes is attributed to the operator's Linear " +
			"account, and it additionally requires agent.linear_proxy.allow_write. Narrowable per agent with " +
			"--scope linear_team=TCL, on the same terms as proxy.linear.read and independently of it, so read and write reach can " +
			"differ. Not default-granted and not owner-implied.",
	},
}

// visiblePermissionRegistry returns the permission catalog exposed to humans
// and agents. Proxy permissions are useful only when the semantic proxy is
// available; advertising them otherwise makes agents mistake an unavailable
// optional feature for missing authority to use their environment's normal
// Git, GitHub, or Linear tooling. Keep the full registry above for validation
// and stored-grant resolution so disabling the proxy never destroys policy.
func visiblePermissionRegistry(proxyEnabled bool) []PermSlug {
	out := make([]PermSlug, 0, len(permissionRegistry))
	for _, p := range permissionRegistry {
		if !proxyEnabled && isSemanticProxyPermission(p.Slug) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func isSemanticProxyPermission(slug string) bool {
	switch slug {
	case PermGitRead, PermGitPush, PermGitHubRead, PermGitHubWrite, PermGitHubMerge,
		PermLinearRead, PermLinearWrite:
		return true
	default:
		return false
	}
}

// Permission slugs for the permissions-management endpoints themselves.
// Recursive: an agent that holds permissions.grant can hand out more
// permissions to itself or others. By default no agent holds these, so
// they're effectively human-only.
const (
	PermPermissionsGrant  = "permissions.grant"
	PermPermissionsRevoke = "permissions.revoke"
)

// IsKnownPermSlug reports whether slug is registered in
// permissionRegistry. Used by the grant validator to reject typos.
func IsKnownPermSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return true
		}
	}
	return false
}

// IsGroupsAdminImpliedSlug reports whether groups.admin is an umbrella for
// slug. The registry deliberately reserves the groups.* namespace for group
// administration, so newly registered operations join
// the umbrella automatically instead of requiring a second hand-maintained
// list. The umbrella never implies itself.
func IsGroupsAdminImpliedSlug(slug string) bool {
	return slug != PermGroupsAdmin && strings.HasPrefix(slug, "groups.")
}

// IsOwnerImpliedSlug reports whether group ownership structurally confers
// slug (see PermSlug.OwnerImplied). Used by the CLI permission listing to
// surface owner-conferred capabilities for an owner agent.
func IsOwnerImpliedSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.OwnerImplied
		}
	}
	return false
}

// IsMemberImpliedSlug reports whether active membership in the action's
// target group structurally confers slug.
func IsMemberImpliedSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.MemberImplied
		}
	}
	return false
}

// GroupSiblingForSlug returns the group-scoped alternative for a global
// agent capability, or "" when the slug has no paired group-member form.
func GroupSiblingForSlug(slug string) string {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.GroupSibling
		}
	}
	return ""
}

// OwnerImpliedSlugs returns the sorted set of slugs group ownership
// confers structurally (PermSlug.OwnerImplied). Stable order so callers
// (CLI, tests) get deterministic output.
func OwnerImpliedSlugs() []string {
	var out []string
	for _, p := range permissionRegistry {
		if p.OwnerImplied {
			out = append(out, p.Slug)
		}
	}
	sort.Strings(out)
	return out
}

// IsAutoGrantableSlug reports whether the approval popup may persist slug
// from its "Always allow for this agent" button (see PermSlug.AutoGrantable).
// The popup gates BOTH the button's visibility and its server-side persist
// on this, so an unknown or ineligible slug can never be self-granted from
// a scraped popup URL.
func IsAutoGrantableSlug(slug string) bool {
	for _, p := range permissionRegistry {
		if p.Slug == slug {
			return p.AutoGrantable
		}
	}
	return false
}

// AutoGrantableSlugs returns the sorted set of slugs eligible for the
// popup's "always allow" persist (PermSlug.AutoGrantable). Stable order so
// callers (tests) get deterministic output.
func AutoGrantableSlugs() []string {
	var out []string
	for _, p := range permissionRegistry {
		if p.AutoGrantable {
			out = append(out, p.Slug)
		}
	}
	sort.Strings(out)
	return out
}

// permissionsState mirrors the data behind the GET /v1/permissions
// view. Defaults come from config.json (granted to all agents); grants
// come from SQLite (table agent_permissions), keyed by full conv-id,
// and ADD to defaults rather than replace them.
//
// Overrides is the full tri-state per-conv view — conv-id → slug →
// "grant" | "deny". Grants (above) is the grant-only projection of the
// same table, kept for back-compat with readers that predate deny.
//
// AgentIDs projects the stable agent_id behind each conv key in
// Grants/Overrides (conv-id → agent_id), so the CLI roster can LEAD with
// the rotation-immune id (`name (agt_xxxxxxxx)`) while the maps stay
// conv-keyed on the wire (JOH-325). Absent for a conv that doesn't (yet)
// resolve to an actor; readers fall back to the conv prefix then.
//
// Titles is the display-name projection of the same keys (conv-id →
// display title). It exists so an agent-side CLI can render the roster
// without reading ~/.tclaude/data itself — a sandboxed agent is denied
// that directory by design, so decoration has to arrive over the wire
// (TCL-611). Absent for a conv with no index row; readers render blank.
//
// GroupGrants is the third standing source the resolver consults (group
// name → slugs granted to every member). It used to be absent from this
// view entirely, which made the roster read as the whole picture when it
// was not: an agent could hold a slug through its group with nothing here
// or in the effective listing saying so.
type permissionsState struct {
	Defaults    []string                              `json:"defaults"`
	Grants      map[string][]string                   `json:"grants"`
	Overrides   map[string]map[string]string          `json:"overrides"`
	Scopes      map[string]map[string]PermissionScope `json:"scopes,omitempty"`
	GroupGrants map[string][]string                   `json:"group_grants"`
	GroupScopes map[string]map[string]PermissionScope `json:"group_scopes,omitempty"`
	AgentIDs    map[string]string                     `json:"agent_ids"`
	Titles      map[string]string                     `json:"titles"`
}

// permissionsEffectiveResp is the daemon-resolved answer to
// `GET /v1/permissions?target=<selector>` — everything
// `tclaude agent permissions ls <target>` renders, with selector
// resolution and the effective/owner-implied calculation done here
// rather than in the CLI (TCL-611).
type permissionsEffectiveResp struct {
	// Resolved is the explicit contract discriminator, always true here.
	// A pre-TCL-611 daemon ignores `?target` and answers the same GET with
	// the ordinary roster (HTTP 200), which decodes into this struct as
	// all-zero — an empty effective set that would materially misstate the
	// target's authority. Clients MUST refuse a response without this flag
	// and tell the operator to restart agentd.
	Resolved bool `json:"resolved"`
	// Target echoes the selector as typed.
	Target string `json:"target"`
	// TargetKey is the resolved conv-id; empty for the "default" sentinel.
	TargetKey string `json:"target_key,omitempty"`
	// AgentID is the stable actor key behind TargetKey, for leading the
	// rendered "who" (JOH-325); empty when the conv is not an agent.
	AgentID string `json:"agent_id,omitempty"`
	// Title is the resolved conv's display title, when known.
	Title string `json:"title,omitempty"`
	// Effective is every slug the gate would currently allow, sorted —
	// computed by asking the gate's own resolver per slug, so it covers
	// sudo elevations and group-granted slugs as well as defaults,
	// per-conv overrides and the owner bypass.
	Effective []string `json:"effective"`
	// Source names the matched inputs, e.g. "defaults+grants:<conv>+group".
	Source string `json:"source"`
	// OwnerImplied is the subset of Effective contributed SOLELY by group
	// ownership, so the CLI can annotate those rows "(via ownership)".
	OwnerImplied []string `json:"owner_implied"`
	// Provenance maps each effective slug to the resolver source that
	// granted it — "sudo", "override", "group", "default", or "owner".
	// It comes straight from the gate's own
	// verdict, so the listing explains a decision without a second model
	// of the precedence.
	Provenance map[string]string `json:"provenance,omitempty"`
	// OwnedGroups names the ACTIVE groups this target owns, so a client
	// can say WHERE an owner-conferred slug applies. Empty for a
	// non-owner. Owner-implied slugs that declare the group dimension receive
	// one scope per name; owner-implied slugs without it are global bonuses.
	OwnedGroups []string `json:"owned_groups,omitempty"`
}

// targetSentinelDefault is the magic target string that means "modify
// the DefaultPermissions list" rather than a per-conv override. Chosen
// so it can't collide with a real conv-id (no UUID is "default") and
// reads well in CLI invocations.
const targetSentinelDefault = "default"

// resolvedTarget is the result of normalising a permissions target into
// a storage handle. For sentinel "default" the kind is sentinel and key
// is "". For a conv selector, key is the full conv-id.
type resolvedTarget struct {
	Sentinel  bool
	Key       string // full conv-id when !Sentinel
	AgentID   string // stable agent_id behind Key, for leading the response "who" (JOH-325); "" when not an actor
	ConvTitle string // best-effort display title for the resolved conv (echoed in responses)
}

func resolveTarget(target string) (*resolvedTarget, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("target is required (use %q for the default-permissions list, or a conv selector)", targetSentinelDefault)
	}
	if target == targetSentinelDefault {
		return &resolvedTarget{Sentinel: true}, nil
	}
	res, _, err := agent.ResolveSelector(target)
	if err != nil {
		return nil, err
	}
	rt := &resolvedTarget{Key: res.ConvID, AgentID: res.AgentID}
	if res.Row != nil {
		rt.ConvTitle = agent.DisplayTitle(res.Row)
	}
	return rt, nil
}

// snapshotPermissions returns the current persisted state, joining
// config defaults with the SQLite overrides table.
func snapshotPermissions() (permissionsState, error) {
	cfg, _ := config.Load()
	out := permissionsState{
		Grants:      map[string][]string{},
		Overrides:   map[string]map[string]string{},
		Scopes:      map[string]map[string]PermissionScope{},
		GroupGrants: map[string][]string{},
		GroupScopes: map[string]map[string]PermissionScope{},
		AgentIDs:    map[string]string{},
		Titles:      map[string]string{},
	}
	if cfg != nil && cfg.Agent != nil {
		out.Defaults = append(out.Defaults, cfg.Agent.DefaultPermissions...)
	}
	grants, err := db.ListAllAgentPermissions()
	if err != nil {
		return out, err
	}
	if grants != nil {
		out.Grants = grants
	}
	overrides, err := db.ListAllAgentPermissionOverrides()
	if err != nil {
		return out, err
	}
	if overrides != nil {
		out.Overrides = overrides
	}
	scopes, err := db.ListAllAgentPermissionScopes()
	if err != nil {
		return out, err
	}
	for convID, bySlug := range scopes {
		for slug, raw := range bySlug {
			scope := permissionScopeFromJSON(raw)
			if len(scope) == 0 {
				continue
			}
			if out.Scopes[convID] == nil {
				out.Scopes[convID] = map[string]PermissionScope{}
			}
			out.Scopes[convID][slug] = scope
		}
	}
	// Group grants are a standing source in their own right — surface them
	// so the roster shows every place a slug can come from. An unreadable
	// group is skipped rather than failing the whole view.
	groups, err := db.ListAgentGroups()
	if err != nil {
		// Degrade rather than 500 the whole view — the defaults and
		// per-agent overrides below are still worth answering with, and
		// the targeted effective view reads group grants itself.
		slog.Warn("permissions: group listing failed; group grants omitted", "error", err)
		groups = nil
	}
	for _, g := range groups {
		// An archived group grants nothing — the resolver's join requires
		// archived_at IS NULL — so listing its slugs would overstate.
		if g == nil || g.IsArchived() {
			continue
		}
		grants, err := db.ListAgentGroupPermissionRows(g.ID)
		if err != nil {
			slog.Warn("permissions: group grant read failed", "group", g.Name, "error", err)
			continue
		}
		if len(grants) > 0 {
			slugs := make([]string, 0, len(grants))
			for _, grant := range grants {
				slugs = append(slugs, grant.Slug)
				if scope := permissionScopeFromJSON(grant.ScopeJSON); len(scope) != 0 {
					if out.GroupScopes[g.Name] == nil {
						out.GroupScopes[g.Name] = map[string]PermissionScope{}
					}
					out.GroupScopes[g.Name][grant.Slug] = scope
				}
			}
			sort.Strings(slugs)
			out.GroupGrants[g.Name] = slugs
		}
	}
	// Project the stable agent_id behind every conv key so the CLI roster
	// can lead with it (display-only — the maps stay conv-keyed). Resolve
	// each conv once; a key that doesn't map to an actor is simply absent.
	for conv := range out.Grants {
		addAgentIDProjection(out.AgentIDs, conv)
		addTitleProjection(out.Titles, conv)
	}
	for conv := range out.Overrides {
		addAgentIDProjection(out.AgentIDs, conv)
		addTitleProjection(out.Titles, conv)
	}
	return out, nil
}

// addTitleProjection records conv → display title in dst so the CLI can
// decorate the roster without its own conv_index read (TCL-611 — a
// sandboxed agent cannot open the private DB). Accepts full conv-ids and
// the prefixes that occasionally show up as grant keys, mirroring what
// the CLI used to try locally. Best-effort: an unresolvable key is simply
// left out and renders blank.
func addTitleProjection(dst map[string]string, conv string) {
	if len(conv) < 8 {
		return
	}
	if _, ok := dst[conv]; ok {
		return
	}
	if row, err := db.GetConvIndex(conv); err == nil && row != nil {
		if title := agent.DisplayTitle(row); title != "" {
			dst[conv] = title
		}
		return
	}
	if row, err := db.FindConvIndexByPrefix(conv); err == nil && row != nil {
		if title := agent.DisplayTitle(row); title != "" {
			dst[conv] = title
		}
	}
}

// addAgentIDProjection records conv → agent_id in dst, skipping convs
// already resolved or with no actor behind them. Best-effort: a lookup
// error leaves the conv out, and the reader falls back to the conv prefix.
func addAgentIDProjection(dst map[string]string, conv string) {
	if conv == "" {
		return
	}
	if _, ok := dst[conv]; ok {
		return
	}
	if agentID, err := db.AgentIDForConv(conv); err == nil && agentID != "" {
		dst[conv] = agentID
	}
}

// addDefaultPermission inserts slug into config.Agent.DefaultPermissions
// (creating the section if missing). Idempotent — slug already present
// is a no-op.
func addDefaultPermission(slug string) error {
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	for _, s := range cfg.Agent.DefaultPermissions {
		if s == slug {
			return nil
		}
	}
	cfg.Agent.DefaultPermissions = append(cfg.Agent.DefaultPermissions, slug)
	sort.Strings(cfg.Agent.DefaultPermissions)
	return config.Save(cfg)
}

// removeDefaultPermission removes slug from DefaultPermissions. No-op
// if absent. Empty list is preserved (we don't delete the agent
// section just because the list emptied — that would make subsequent
// adds noisier in the diff).
func removeDefaultPermission(slug string) error {
	cfg, _ := config.Load()
	if cfg == nil || cfg.Agent == nil {
		return nil
	}
	out := cfg.Agent.DefaultPermissions[:0]
	for _, s := range cfg.Agent.DefaultPermissions {
		if s != slug {
			out = append(out, s)
		}
	}
	cfg.Agent.DefaultPermissions = out
	return config.Save(cfg)
}

// handlePermissions dispatches GET /v1/permissions. Read-only: anyone
// (including agents with no granted slugs) can introspect the current
// state. Writing happens at /v1/permissions/grant and .../revoke.
//
// With `?target=<selector>` the daemon additionally resolves the selector
// and returns the effective view for that one target
// (permissionsEffectiveResp). That branch exists so the CLI never has to
// touch ~/.tclaude/data itself: a sandboxed agent is denied that
// directory, and the client-side fallback used to degrade into a
// conversation-index rescan, a warning storm and a raw filesystem path in
// the error (TCL-611).
func handlePermissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	state, err := snapshotPermissions()
	if err != nil {
		// The underlying error can carry private DB paths; keep those
		// server-side and hand the caller a generic typed failure.
		slog.Error("permissions: snapshot failed", "error", err)
		writeError(w, http.StatusInternalServerError, "server", "permission state unavailable")
		return
	}
	if target := strings.TrimSpace(r.URL.Query().Get("target")); target != "" {
		writeEffectivePermissions(w, r, state, target)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// writeEffectivePermissions resolves target and writes the effective
// permission view for it. Unknown selectors get a concise typed
// `not_found`; ambiguous ones a typed `ambiguous` with candidates, the
// same envelope /v1/lookup and /v1/messages already use.
func writeEffectivePermissions(w http.ResponseWriter, r *http.Request, state permissionsState, target string) {
	if target == targetSentinelDefault {
		defs := append([]string{}, state.Defaults...)
		sort.Strings(defs)
		writeJSON(w, http.StatusOK, permissionsEffectiveResp{
			Resolved:     true,
			Target:       targetSentinelDefault,
			Effective:    defs,
			Source:       "defaults",
			OwnerImplied: []string{},
		})
		return
	}
	// `.` / `-` mean "the conversation invoking this command". The CLI used
	// to expand them from its own process before resolving; now that the
	// selector travels to the daemon, they must be resolved from the
	// AUTHENTICATED PEER instead — letting agent.ResolveSelector see them
	// here would inspect the daemon's own process identity and answer for
	// the wrong conversation (or nothing at all).
	selector := target
	if selector == "." || selector == "-" {
		callerConv, isHuman, ok := authedCaller(w, r)
		if !ok {
			return // authedCaller already wrote the refusal
		}
		if isHuman || callerConv == "" {
			writeError(w, http.StatusNotFound, "not_found",
				fmt.Sprintf("%q means the calling conversation; this invocation has none — pass a conv selector, or %q for the defaults list",
					target, targetSentinelDefault))
			return
		}
		selector = callerConv
	}
	res, matches, err := agent.ResolveSelector(selector)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      fmt.Sprintf("selector %q matches multiple conversations", target),
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		// err may wrap an internal DB/filesystem error; log it with context
		// and answer with the domain fact the caller is entitled to.
		slog.Warn("permissions: selector did not resolve", "target", target, "error", err)
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no conversation or agent matches %q", target))
		return
	}
	resp := permissionsEffectiveResp{
		Resolved:  true,
		Target:    target,
		TargetKey: res.ConvID,
		AgentID:   res.AgentID,
	}
	if res.Row != nil {
		resp.Title = agent.DisplayTitle(res.Row)
	}
	effective, ownerAdded, provenance, source := effectivePermsFor(state, res.ConvID, ownerImpliedSlugsFor(res.ConvID))
	sort.Strings(effective)
	sort.Strings(ownerAdded)
	resp.Effective = effective
	resp.OwnerImplied = ownerAdded
	resp.Provenance = provenance
	resp.Source = source
	if resp.Effective == nil {
		resp.Effective = []string{}
	}
	if resp.OwnerImplied == nil {
		resp.OwnerImplied = []string{}
	}
	if resp.Provenance == nil {
		resp.Provenance = map[string]string{}
	}
	resp.OwnedGroups = ownedGroupNamesFor(res.ConvID)
	writeJSON(w, http.StatusOK, resp)
}

// ownerImpliedSlugsFor returns the owner-conferred tier for convID —
// non-empty only when the conv owns at least one group. Ownership is a
// derived grant (PermSlug.OwnerImplied), so an owner effectively holds these
// without an explicit assignment, CONFINED by each owned group's own
// owner-scope map (TCL-1071). A DB error degrades to "not an owner": owner
// perms go un-annotated rather than failing the whole listing.
func ownerImpliedSlugsFor(convID string) ownerImpliedTier {
	return ownerImpliedTierFor(convID)
}

// ownedGroupNamesFor returns the names of the ACTIVE groups convID owns,
// sorted. It is what lets a client say WHERE an owner-conferred slug
// applies ("via ownership of: dev, qa") instead of the bare "via
// ownership", which reads as fleet-wide authority the gate never grants.
// An archived group is skipped: its endpoints reject mutation, so naming
// it would promise reach the owner does not have.
func ownedGroupNamesFor(convID string) []string {
	owned, err := db.ListGroupsOwnedBy(convID)
	if err != nil {
		slog.Warn("permissions: owned-group lookup failed", "conv", convID, "error", err)
		return nil
	}
	var names []string
	for _, id := range owned {
		g, err := db.GetAgentGroupByID(id)
		if err != nil || g == nil || g.IsArchived() {
			continue
		}
		names = append(names, g.Name)
	}
	sort.Strings(names)
	return names
}

// effectivePermsFor answers "what would the gate decide for this agent",
// slug by slug, by asking the gate's own resolver
// (resolvePermissionVerdict) about every candidate slug and applying the
// owner-derived grant exactly where requirePermissionEx applies it: at the
// permUndecided gap, never over an explicit deny.
//
// It deliberately does NOT re-derive the precedence from `state`. The
// listing used to union (defaults ∪ per-conv grants ∪ owner-implied) and
// subtract denies, which silently omitted the resolver's other two
// sources — group-granted slugs and active sudo elevations — so an agent
// could hold human.notify through its group while `permissions ls`
// reported it absent. Routing both readers through one resolver means a
// source added there cannot drift out of the listing.
//
// `state` now contributes only the defaults list (the resolver takes the
// default as a parameter, as the request path does) and the candidate
// enumeration; ownerImplied is empty for a non-owner.
//
// ownerAdded reports the subset held SOLELY via an owner-derived grant, so the
// caller can annotate those rows "(via ownership)". provenance maps every
// effective slug to the source that granted it ("sudo", "override",
// "group", "default", "owner").
//
// The returned label names the matched sources ("defaults",
// "defaults+grants:<conv>", "+group", "+sudo", "+owner", with " −denies"
// appended when any deny override applies).
func effectivePermsFor(state permissionsState, convID string, ownerImplied ownerImpliedTier) (effective, ownerAdded []string, provenance map[string]string, source string) {
	defaults := map[string]bool{}
	for _, s := range state.Defaults {
		defaults[s] = true
	}
	provenance = map[string]string{}
	matched := map[permSource]bool{}
	anyDeny := false
	// One read of the agent's standing sources, then the SAME precedence
	// the gate applies, per candidate slug. Loading once is what keeps the
	// dashboard roster (this runs per agent, per snapshot poll) from
	// issuing a query per slug per agent.
	src := loadPermSources(convID)
	for _, slug := range candidatePermissionSlugs(state, convID, ownerImplied, src) {
		v := resolveEffectivePermissionVerdictFrom(src, slug,
			defaults[slug], defaults[PermGroupsAdmin])
		switch v.Resolution {
		case permAllow:
			effective = append(effective, slug)
			provenance[slug] = permissionProvenance(v.Source, v.ScopeJSON)
			matched[v.Source] = true
			// A scoped higher-tier grant and owner-derived group scopes are
			// additive at the action gate. Preserve both in the effective view;
			// otherwise an owner with explicit B + owned A would be shown only B.
			if permissionVerdictIsNarrowed(v) {
				if entry, ok := ownerImplied[slug]; ok && entry.confers() {
					provenance[slug] += " OR " + ownerProvenance(slug, entry)
					matched[permSourceOwner] = true
				}
			}
		case permUndecided:
			// The owner-derived grant fills exactly this gap — see
			// requirePermissionEx, where it is consulted only for
			// permUndecided so an explicit deny stays authoritative.
			// entry.confers() rather than mere presence: a DEGRADED entry
			// (a group whose narrowing could not be read) authorizes nothing
			// at the gate, so reporting the slug as effective here would be
			// exactly the listing-vs-gate drift the shared resolver exists to
			// prevent.
			if entry, ok := ownerImplied[slug]; ok && entry.confers() {
				effective = append(effective, slug)
				ownerAdded = append(ownerAdded, slug)
				// Carry the SCOPE, not just "owner": a reader told only
				// that ownership confers the slug will assume it reaches
				// the whole fleet, when the gate confines most of these
				// to owned groups or their members — and, since TCL-1071,
				// possibly to a narrowed slice of one owned group.
				provenance[slug] = ownerProvenance(slug, entry)
				matched[permSourceOwner] = true
			} else if memberImpliedForAgent(convID, slug) {
				effective = append(effective, slug)
				provenance[slug] = "member:group"
				matched[permSourceMember] = true
			}
		case permDeny:
			anyDeny = true
		}
	}
	source = "defaults"
	if matched[permSourceOverride] {
		source += "+grants:" + convID
	}
	if matched[permSourceGroup] {
		source += "+group"
	}
	if matched[permSourceSudo] {
		source += "+sudo"
	}
	if matched[permSourceOwner] {
		source += "+owner"
	}
	if matched[permSourceMember] {
		source += "+member"
	}
	if anyDeny {
		source += " −denies"
	}
	return effective, ownerAdded, provenance, source
}

func permissionVerdictIsNarrowed(v permVerdict) bool {
	if len(v.ScopeJSON) == 0 {
		return false
	}
	for _, scope := range v.ScopeJSON {
		if strings.TrimSpace(scope) == "" {
			return false
		}
	}
	return true
}

func memberImpliedForAgent(convID, slug string) bool {
	if !IsMemberImpliedSlug(slug) {
		return false
	}
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if !g.IsArchived() {
			return true
		}
	}
	return false
}

// ownerProvenance renders the provenance value for a slug held via the
// owner bypass: "owner:<scope>", so a client can phrase the reach
// ("of: dev, qa" vs "members only" vs unscoped) without its own copy of
// the registry. A slug with no registered scope degrades to plain
// "owner", which older clients already render as "(via ownership)".
//
// A per-group NARROWING (TCL-1071) is appended in the same bracketed form
// grant scopes use — "owner:group [spawn_profile=p1]" — so the listing states
// the reach the gate will actually allow instead of the unrestricted bypass
// the bare source name implies. Unrestricted ownership renders exactly as
// before, so no existing client output changes.
func ownerProvenance(_ string, entry ownerTierEntry) string {
	return string(permSourceOwner) + ownerScopeDisplay(entry)
}

// candidatePermissionSlugs enumerates every slug worth asking the
// resolver about for this agent: the known registry, plus any slug some
// source mentions. The extra sources matter because the daemon stores
// forward-compat slugs a given build's registry may not know yet, and a
// slug that is genuinely in force must be listed even then.
func candidatePermissionSlugs(state permissionsState, convID string, ownerImplied ownerImpliedTier, src permSources) []string {
	seen := map[string]bool{}
	var out []string
	add := func(slugs ...string) {
		for _, s := range slugs {
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	for _, s := range permissionRegistry {
		add(s.Slug)
	}
	add(state.Defaults...)
	add(state.Grants[convID]...)
	for slug := range state.Overrides[convID] {
		add(slug)
	}
	add(ownerImplied.slugs()...)
	// The agent's own sources, already read — these are what carry a slug
	// this build's registry may not know.
	for slug := range src.group {
		add(slug)
	}
	for slug := range src.sudo {
		add(slug)
	}
	for slug := range src.override {
		add(slug)
	}
	sort.Strings(out)
	return out
}

// handlePermissionsSlugs returns the registry of known slugs +
// descriptions. Open to anyone — same shape as the agent-coord skill's
// docs, just queryable.
func handlePermissionsSlugs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	out := visiblePermissionRegistry(gitProxyRoutesEnabled(r))
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	writeJSON(w, http.StatusOK, out)
}

type permissionsMutateReq struct {
	Target string          `json:"target"`
	Slug   string          `json:"slug"`
	Scope  json.RawMessage `json:"scope,omitempty"`
}

type permissionsMutateResp struct {
	Target    string          `json:"target"`
	TargetKey string          `json:"target_key,omitempty"` // resolved conv-id when target != "default"
	AgentID   string          `json:"agent_id,omitempty"`   // stable agent_id behind TargetKey, for leading the "who" (JOH-325)
	Title     string          `json:"title,omitempty"`      // display title of the resolved conv, when known
	Slug      string          `json:"slug"`
	Effect    string          `json:"effect,omitempty"` // post-mutation override effect: "grant", "deny", or "default" (cleared)
	Scope     PermissionScope `json:"scope,omitempty"`
	Effective []string        `json:"effective"` // post-mutation GRANTED slug list for that target
}

func decodeMutateReq(w http.ResponseWriter, r *http.Request) (*permissionsMutateReq, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return nil, false
	}
	var body permissionsMutateReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return nil, false
	}
	body.Target = strings.TrimSpace(body.Target)
	body.Slug = strings.TrimSpace(body.Slug)
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("target is required (use %q for the default-permissions list, or a conv selector)", targetSentinelDefault))
		return nil, false
	}
	if body.Slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "slug is required")
		return nil, false
	}
	return &body, true
}

// granterLabel describes who is granting / revoking. For humans we
// record "<human>"; for agents we use their conv-id. Logged into
// agent_permissions.granted_by for audit.
func granterLabel(granterConvID string) string {
	if granterConvID == "" {
		return "<human>"
	}
	return granterConvID
}

// auditedCaller composes the granted_by audit string for a permission-
// gated mutate, annotating sudo-elevated calls so forensic queries
// can tell normal ops apart from elevated ones.
//
// Returns:
//   - "" when callerConvID is empty (human path; sites that label
//     humans differently — e.g. granterLabel — keep doing so).
//   - "<conv>:via-sudo:grant-id=<n>" when the call only passed because
//     of an active sudo grant for perm. The grant-id ties the audit
//     string to a specific row in agent_sudo_grants, so a
//     post-incident query like "what did agent X do during grant
//     42's window?" is a single LIKE.
//   - "<conv>" otherwise — the agent had a non-sudo source for the
//     permission (a per-conv grant override or the default-permissions
//     list). Annotating those with via-sudo would be misleading.
//
// Only used at the audit-write layer, not in the hot read path —
// re-checking config + DB here is fine.
func auditedCaller(callerConvID, perm string) string {
	return auditedCallerWithSudoGrant(callerConvID, perm, 0)
}

func auditedCallerWithSudoGrant(callerConvID, perm string, decisionGrantID int64) string {
	if callerConvID == "" {
		return ""
	}
	if decisionGrantID > 0 {
		return fmt.Sprintf("%s:via-sudo:grant-id=%d", callerConvID, decisionGrantID)
	}
	cfg, _ := config.Load()
	src := loadPermSources(callerConvID)
	// First resolve without the sudo tier. If standing authority (including a
	// group grant or the groups.admin umbrella) suffices, sudo was not
	// load-bearing and must not be stamped onto the operation.
	withoutSudo := src
	withoutSudo.sudo = map[string]sudoPermSource{}
	standing := resolveEffectivePermissionVerdictFrom(withoutSudo, perm,
		cfg.HasDefaultPermission(perm), cfg.HasDefaultPermission(PermGroupsAdmin))
	if contextFreeResolution(standing) == permAllow {
		return callerConvID
	}
	// Re-resolve with sudo. The winning effective verdict may be either the
	// exact operation slug or groups.admin; carrying SudoGrantID from that
	// verdict preserves the umbrella elevation's forensic provenance.
	effective := resolveEffectivePermissionVerdictFrom(src, perm,
		cfg.HasDefaultPermission(perm), cfg.HasDefaultPermission(PermGroupsAdmin))
	if contextFreeResolution(effective) != permAllow || effective.SudoGrantID == 0 {
		return callerConvID
	}
	return fmt.Sprintf("%s:via-sudo:grant-id=%d", callerConvID, effective.SudoGrantID)
}

// handlePermissionsGrant adds slug to either the DefaultPermissions list
// (target=="default", in config.json) or to agent_permissions(conv_id,
// slug) in SQLite. Idempotent.
//
// Refuses unknown slugs with a 400 listing the registered ones. The registry
// is the sole vocabulary shared by grants, dashboard controls, sudo, imports,
// and authorization gates.
func handlePermissionsGrant(w http.ResponseWriter, r *http.Request) {
	granter, ok := requirePermission(w, r, PermPermissionsGrant)
	if !ok {
		return
	}
	body, ok := decodeMutateReq(w, r)
	if !ok {
		return
	}
	if !IsKnownPermSlug(body.Slug) {
		writeError(w, http.StatusBadRequest, "unknown_slug",
			fmt.Sprintf("unknown permission slug %q. Known slugs: %s.",
				body.Slug, strings.Join(knownSlugs(), ", ")))
		return
	}
	scope, scopeJSON, err := parsePermissionScope(body.Scope)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	if err := validatePermissionScopeForSlug(body.Slug, scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	target, err := resolveTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	// Attenuation-only delegation: an AGENT granter may not hand out a scope
	// wider than its own for this slug. granter is "" for the human operator,
	// who is unconstrained. Selector-bearing target_agent scopes additionally
	// require this resolved conferee to be the granter or one of its
	// descendants. This also covers the sentinel "default" target below:
	// adding a slug there confers it UNSCOPED to every agent, which a scoped
	// granter can never cover.
	conferee := grantConferee{agentID: target.AgentID}
	if err := checkGrantAttenuation(granter, conferee, []conferredGrant{{Slug: body.Slug, Scope: scopeJSON}}); err != nil {
		writeError(w, http.StatusForbidden, "scope_not_attenuated", err.Error())
		return
	}
	resp := permissionsMutateResp{Target: body.Target, Slug: body.Slug, Effect: db.PermEffectGrant, Scope: scope}
	if target.Sentinel {
		if len(scope) != 0 {
			writeError(w, http.StatusBadRequest, "invalid_scope", "scoped grants are not supported on the default-permissions list")
			return
		}
		if err := addDefaultPermission(body.Slug); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		state, _ := snapshotPermissions()
		resp.Effective = append(resp.Effective, state.Defaults...)
	} else {
		if err := db.GrantAgentPermissionWithScope(target.Key, body.Slug, scopeJSON, granterLabel(granter)); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		resp.TargetKey = target.Key
		resp.AgentID = target.AgentID
		resp.Title = target.ConvTitle
		slugs, _ := db.ListAgentPermissionsForConv(target.Key)
		resp.Effective = slugs
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePermissionsDeny writes a per-conv DENY override into
// agent_permissions(conv_id, slug, effect='deny'). A deny suppresses an
// otherwise default-granted slug for one specific agent — the
// subtractive half of the override model that the additive grant path
// alone could not express.
//
// Unlike grant, deny rejects the "default" sentinel target: there is
// nothing below the defaults list to deny. To remove a slug for every
// agent, revoke it from the defaults list instead.
//
// Gated on permissions.grant — writing a permission override (grant or
// deny) is the same management capability; permissions.revoke only
// covers clearing an override back to the inherited default. Humans
// (and the cookie-authed dashboard) bypass.
func handlePermissionsDeny(w http.ResponseWriter, r *http.Request) {
	granter, ok := requirePermission(w, r, PermPermissionsGrant)
	if !ok {
		return
	}
	body, ok := decodeMutateReq(w, r)
	if !ok {
		return
	}
	if scope, _, err := parsePermissionScope(body.Scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	} else if len(scope) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_scope", "deny overrides cannot carry a scope")
		return
	}
	if !IsKnownPermSlug(body.Slug) {
		writeError(w, http.StatusBadRequest, "unknown_slug",
			fmt.Sprintf("unknown permission slug %q. Known slugs: %s.",
				body.Slug, strings.Join(knownSlugs(), ", ")))
		return
	}
	if body.Target == targetSentinelDefault {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"cannot deny on the \"default\" target — deny is a per-conv override. "+
				"To remove a slug for every agent, revoke it from the defaults list instead.")
		return
	}
	target, err := resolveTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := db.SetAgentPermissionOverride(target.Key, body.Slug, db.PermEffectDeny, granterLabel(granter)); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	slugs, _ := db.ListAgentPermissionsForConv(target.Key)
	writeJSON(w, http.StatusOK, permissionsMutateResp{
		Target:    body.Target,
		TargetKey: target.Key,
		AgentID:   target.AgentID,
		Title:     target.ConvTitle,
		Slug:      body.Slug,
		Effect:    db.PermEffectDeny,
		Effective: slugs,
	})
}

// handlePermissionsRevoke removes slug from either DefaultPermissions
// (config.json) or agent_permissions for the resolved conv. For a
// per-conv target it clears whichever override (grant or deny) is
// present, returning the slug to its inherited default. Idempotent.
func handlePermissionsRevoke(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermPermissionsRevoke)
	if !ok {
		return
	}
	body, ok := decodeMutateReq(w, r)
	if !ok {
		return
	}
	if scope, _, err := parsePermissionScope(body.Scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	} else if len(scope) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_scope", "revoke does not accept a scope")
		return
	}
	target, err := resolveTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if selfScopeShedRefused(w, caller, target, body.Slug) {
		return
	}
	resp := permissionsMutateResp{Target: body.Target, Slug: body.Slug, Effect: "default"}
	if target.Sentinel {
		if err := removeDefaultPermission(body.Slug); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		state, _ := snapshotPermissions()
		resp.Effective = append(resp.Effective, state.Defaults...)
	} else {
		if _, err := db.RevokeAgentPermission(target.Key, body.Slug); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		resp.TargetKey = target.Key
		resp.AgentID = target.AgentID
		resp.Title = target.ConvTitle
		slugs, _ := db.ListAgentPermissionsForConv(target.Key)
		resp.Effective = slugs
	}
	writeJSON(w, http.StatusOK, resp)
}

func knownSlugs() []string {
	out := make([]string, len(permissionRegistry))
	for i, p := range permissionRegistry {
		out[i] = p.Slug
	}
	sort.Strings(out)
	return out
}
